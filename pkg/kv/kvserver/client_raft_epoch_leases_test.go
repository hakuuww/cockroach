// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// Raft related tests that are very tightly coupled to epoch based leases. An
// equivalent leader leases variant of these tests should exist elsewhere. As
// such, we should be able to get rid of these tests once epoch based leases are
// no longer supported.

// TestRaftCheckQuorumEpochLeases tests that Raft CheckQuorum works properly
// with epoch based leases, i.e. that a leader will step down if it hasn't heard
// from a quorum of followers in the last election timeout interval.
//
//	               n1 (leader)
//	              x  x
//	             x    x
//	(follower) n2 ---- n3 (follower)
//
// We test this by partitioning the leader away from two followers, using either
// a symmetric or asymmetric partition. In the asymmetric case, the leader can
// send heartbeats to followers, but won't receive responses. Eventually, it
// should step down and the followers should elect a new leader.
//
// We also test this with both quiesced and unquiesced ranges.
func TestRaftCheckQuorumEpochLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test is timing-sensitive, so skip it under deadlock detector and
	// race.
	skip.UnderDeadlock(t)
	skip.UnderRace(t)

	testutils.RunTrueAndFalse(t, "symmetric", func(t *testing.T, symmetric bool) {
		testutils.RunTrueAndFalse(t, "quiesce", func(t *testing.T, quiesce bool) {
			ctx := context.Background()

			// Only run the test with epoch based leases.
			st := cluster.MakeTestingClusterSettings()
			kvserver.OverrideDefaultLeaseType(ctx, &st.SV, roachpb.LeaseEpoch)

			tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{
				ReplicationMode: base.ReplicationManual,
				ServerArgs: base.TestServerArgs{
					Settings: st,
					RaftConfig: base.RaftConfig{
						RaftEnableCheckQuorum: true,
						RaftTickInterval:      200 * time.Millisecond, // speed up test
					},
					Knobs: base.TestingKnobs{
						Store: &kvserver.StoreTestingKnobs{
							DisableQuiescence: !quiesce,
						},
					},
				},
			})
			defer tc.Stopper().Stop(ctx)

			logStatus := func(s *raft.Status) {
				t.Helper()
				require.NotNil(t, s)
				t.Logf("n%d %s at term=%d commit=%d", s.ID, s.RaftState, s.Term, s.Commit)
			}

			// Create a range, upreplicate it, and replicate a write.
			sender := tc.GetFirstStoreFromServer(t, 0).TestSender()
			key := tc.ScratchRange(t)
			desc := tc.AddVotersOrFatal(t, key, tc.Targets(1, 2)...)

			_, pErr := kv.SendWrapped(ctx, sender, incrementArgs(key, 1))
			require.NoError(t, pErr.GoError())
			tc.WaitForValues(t, key, []int64{1, 1, 1})

			repl1, err := tc.GetFirstStoreFromServer(t, 0).GetReplica(desc.RangeID)
			require.NoError(t, err)
			repl2, err := tc.GetFirstStoreFromServer(t, 1).GetReplica(desc.RangeID)
			require.NoError(t, err)
			repl3, err := tc.GetFirstStoreFromServer(t, 2).GetReplica(desc.RangeID)
			require.NoError(t, err)

			// Set up dropping of inbound messages on n1 from n2,n3, but don't
			// activate it yet.
			var partitioned atomic.Bool
			dropRaftMessagesFrom(t, tc.Servers[0], desc, []roachpb.ReplicaID{2, 3}, &partitioned)
			if symmetric {
				// Drop outbound messages from n1 to n2,n3 too.
				dropRaftMessagesFrom(t, tc.Servers[1], desc, []roachpb.ReplicaID{1}, &partitioned)
				dropRaftMessagesFrom(t, tc.Servers[2], desc, []roachpb.ReplicaID{1}, &partitioned)
			}

			// Make sure the lease is on n1 and that everyone has applied it.
			tc.TransferRangeLeaseOrFatal(t, desc, tc.Target(0))
			_, pErr = kv.SendWrapped(ctx, sender, incrementArgs(key, 1))
			require.NoError(t, pErr.GoError())
			tc.WaitForValues(t, key, []int64{2, 2, 2})
			t.Logf("n1 has lease")

			// Wait for the range to quiesce, if enabled.
			if quiesce {
				require.Eventually(t, func() bool {
					return repl1.IsQuiescent() && repl2.IsQuiescent() && repl3.IsQuiescent()
				}, 10*time.Second, 100*time.Millisecond)
				t.Logf("n1, n2, and n3 quiesced")
			} else {
				require.False(t, repl1.IsQuiescent() || repl2.IsQuiescent() || repl3.IsQuiescent())
				t.Logf("n1, n2, and n3 not quiesced")
			}

			// Partition n1.
			partitioned.Store(true)
			t.Logf("n1 partitioned")

			// Fetch the leader's initial status.
			initialStatus := repl1.RaftStatus()
			require.Equal(t, raftpb.StateLeader, initialStatus.RaftState)
			logStatus(initialStatus)

			// Unquiesce the leader if necessary. We have to do so by submitting an
			// empty proposal, otherwise the leader will immediately quiesce again.
			if quiesce {
				ok, err := repl1.MaybeUnquiesceAndPropose()
				require.NoError(t, err)
				require.True(t, ok)
				t.Logf("n1 unquiesced")
			} else {
				require.False(t, repl1.IsQuiescent())
				t.Logf("n1 not quiesced")
			}

			// Wait for the leader to become a candidate.
			require.Eventually(t, func() bool {
				status := repl1.RaftStatus()
				logStatus(status)
				return status.RaftState == raftpb.StatePreCandidate
			}, 10*time.Second, 500*time.Millisecond)
			t.Logf("n1 became pre-candidate")

			// In the case of a symmetric partition of a quiesced range, we have to
			// wake up n2 to elect a new leader.
			if quiesce && symmetric {
				require.True(t, repl2.MaybeUnquiesce())
				t.Logf("n2 unquiesced")
			}

			// n2 or n3 should elect a new leader.
			var leaderStatus *raft.Status
			require.Eventually(t, func() bool {
				for _, status := range []*raft.Status{repl2.RaftStatus(), repl3.RaftStatus()} {
					logStatus(status)
					if status.RaftState == raftpb.StateLeader {
						leaderStatus = status
						return true
					}
				}
				return false
			}, 10*time.Second, 500*time.Millisecond)
			t.Logf("n%d became leader", leaderStatus.ID)

			// n1 should remain pre-candidate, since it doesn't hear about the new
			// leader.
			require.Never(t, func() bool {
				status := repl1.RaftStatus()
				logStatus(status)
				return status.RaftState != raftpb.StatePreCandidate
			}, 3*time.Second, 500*time.Millisecond)
			t.Logf("n1 remains pre-candidate")

			// The existing leader shouldn't have been affected by n1's prevotes.
			var finalStatus *raft.Status
			for _, status := range []*raft.Status{repl2.RaftStatus(), repl3.RaftStatus()} {
				logStatus(status)
				if status.RaftState == raftpb.StateLeader {
					finalStatus = status
					break
				}
			}
			require.NotNil(t, finalStatus)
			require.Equal(t, leaderStatus.ID, finalStatus.ID)
			require.Equal(t, leaderStatus.Term, finalStatus.Term)
		})
	})
}

// TestRequestsOnLaggingReplicaEpochLeases tests that requests sent to a replica
// that's behind in log application don't block. The test indirectly verifies
// that a replica that's not the leader does not attempt to acquire a lease and,
// thus, does not block until it figures out that it cannot, in fact, take the
// lease.
//
// This test relies on follower replicas refusing to forward lease acquisition
// requests to the leader, thereby refusing to acquire a lease. The point of
// this behavior is to prevent replicas that are behind from trying to acquire
// the lease and then blocking traffic for a long time until they find out
// whether they successfully took the lease or not.
//
// The test is run only with epoch based leases.
func TestRequestsOnLaggingReplicaEpochLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	st := cluster.MakeTestingClusterSettings()
	kvserver.OverrideDefaultLeaseType(ctx, &st.SV, roachpb.LeaseEpoch)

	clusterArgs := base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Settings: st,
			// Reduce the election timeout some to speed up the test.
			RaftConfig: base.RaftConfig{RaftElectionTimeoutTicks: 10},
			Knobs: base.TestingKnobs{
				NodeLiveness: kvserver.NodeLivenessTestingKnobs{
					// This test waits for an epoch-based lease to expire, so we're
					// setting the liveness duration as low as possible while still
					// keeping the test stable.
					LivenessDuration: 3000 * time.Millisecond,
					RenewalDuration:  1500 * time.Millisecond,
				},
				Store: &kvserver.StoreTestingKnobs{
					// We eliminate clock offsets in order to eliminate the stasis period
					// of leases, in order to speed up the test.
					MaxOffset: time.Nanosecond,
				},
			},
		},
	}

	tc := testcluster.StartTestCluster(t, 3, clusterArgs)
	defer tc.Stopper().Stop(ctx)

	_, rngDesc, err := tc.Servers[0].ScratchRangeEx()
	require.NoError(t, err)
	key := rngDesc.StartKey.AsRawKey()
	// Add replicas on all the stores.
	tc.AddVotersOrFatal(t, rngDesc.StartKey.AsRawKey(), tc.Target(1), tc.Target(2))

	{
		// Write a value so that the respective key is present in all stores and we
		// can increment it again later.
		_, err := tc.Server(0).DB().Inc(ctx, key, 1)
		require.NoError(t, err)
		log.Infof(ctx, "test: waiting for initial values...")
		tc.WaitForValues(t, key, []int64{1, 1, 1})
		log.Infof(ctx, "test: waiting for initial values... done")
	}

	// Partition the original leader from its followers. We do this by installing
	// unreliableRaftHandler listeners on all three Stores. The handler on the
	// partitioned store filters out all messages while the handler on the other
	// two stores only filters out messages from the partitioned store. The
	// configuration looks like:
	//
	//           [0]
	//          x  x
	//         /    \
	//        x      x
	//      [1]<---->[2]
	//
	log.Infof(ctx, "test: partitioning node")
	const partitionNodeIdx = 0
	partitionStore := tc.GetFirstStoreFromServer(t, partitionNodeIdx)
	partRepl, err := partitionStore.GetReplica(rngDesc.RangeID)
	require.NoError(t, err)
	partReplDesc, err := partRepl.GetReplicaDescriptor()
	require.NoError(t, err)
	partitionedStoreSender := partitionStore.TestSender()
	const otherStoreIdx = 1
	otherStore := tc.GetFirstStoreFromServer(t, otherStoreIdx)
	otherRepl, err := otherStore.GetReplica(rngDesc.RangeID)
	require.NoError(t, err)

	for _, i := range []int{0, 1, 2} {
		store := tc.GetFirstStoreFromServer(t, i)
		h := &unreliableRaftHandler{
			name:                       fmt.Sprintf("store %d", i),
			rangeID:                    rngDesc.RangeID,
			IncomingRaftMessageHandler: store,
		}
		if i != partitionNodeIdx {
			// Only filter messages from the partitioned store on the other two
			// stores.
			h.dropReq = func(req *kvserverpb.RaftMessageRequest) bool {
				return req.FromReplica.StoreID == partRepl.StoreID()
			}
			h.dropHB = func(hb *kvserverpb.RaftHeartbeat) bool {
				return hb.FromReplicaID == partReplDesc.ReplicaID
			}
		}
		store.Transport().ListenIncomingRaftMessages(store.Ident.StoreID, h)
	}

	// Stop the heartbeats so that n1's lease can expire.
	log.Infof(ctx, "test: suspending heartbeats for n1")
	resumeN1Heartbeats := partitionStore.GetStoreConfig().NodeLiveness.PauseAllHeartbeatsForTest()

	// Wait until another replica campaigns and becomes leader, replacing the
	// partitioned one.
	log.Infof(ctx, "test: waiting for leadership transfer")
	testutils.SucceedsSoon(t, func() error {
		// Make sure this replica has not inadvertently quiesced. We need the
		// replica ticking so that it campaigns.
		if otherRepl.IsQuiescent() {
			otherRepl.MaybeUnquiesce()
		}
		lead := otherRepl.RaftStatus().Lead
		if lead == raft.None {
			return errors.New("no leader yet")
		}
		if roachpb.ReplicaID(lead) == partReplDesc.ReplicaID {
			return errors.New("partitioned replica is still leader")
		}
		return nil
	})

	leaderReplicaID := roachpb.ReplicaID(otherRepl.RaftStatus().Lead)
	log.Infof(ctx, "test: the leader is replica ID %d", leaderReplicaID)
	if leaderReplicaID != 2 && leaderReplicaID != 3 {
		t.Fatalf("expected leader to be 1 or 2, was: %d", leaderReplicaID)
	}
	leaderNodeIdx := int(leaderReplicaID - 1)
	leaderNode := tc.Server(leaderNodeIdx)
	leaderStore, err := leaderNode.GetStores().(*kvserver.Stores).GetStore(leaderNode.GetFirstStoreID())
	require.NoError(t, err)

	// Wait until the lease expires.
	log.Infof(ctx, "test: waiting for lease expiration")
	partitionedReplica, err := partitionStore.GetReplica(rngDesc.RangeID)
	require.NoError(t, err)
	testutils.SucceedsSoon(t, func() error {
		status := partitionedReplica.CurrentLeaseStatus(ctx)
		require.True(t,
			status.Lease.OwnedBy(partitionStore.StoreID()), "someone else got the lease: %s", status)
		if status.State == kvserverpb.LeaseState_VALID {
			return errors.New("lease still valid")
		}
		// We need to wait for the stasis state to pass too; during stasis other
		// replicas can't take the lease.
		if status.State == kvserverpb.LeaseState_UNUSABLE {
			return errors.New("lease still in stasis")
		}
		return nil
	})
	log.Infof(ctx, "test: lease expired")

	{
		// Write something to generate some Raft log entries and then truncate the log.
		log.Infof(ctx, "test: incrementing")
		incArgs := incrementArgs(key, 1)
		sender := leaderStore.TestSender()
		_, pErr := kv.SendWrapped(ctx, sender, incArgs)
		require.Nil(t, pErr)
	}

	tc.WaitForValues(t, key, []int64{1, 2, 2})
	index := otherRepl.GetLastIndex()

	// Truncate the log at index+1 (log entries < N are removed, so this includes
	// the increment). This means that the partitioned replica will need a
	// snapshot to catch up.
	log.Infof(ctx, "test: truncating log...")
	truncArgs := &kvpb.TruncateLogRequest{
		RequestHeader: kvpb.RequestHeader{
			Key: key,
		},
		Index:   index,
		RangeID: rngDesc.RangeID,
	}
	{
		_, pErr := kv.SendWrapped(ctx, leaderStore.TestSender(), truncArgs)
		require.NoError(t, pErr.GoError())
	}

	// Resume n1's heartbeats and wait for it to become live again. This is to
	// ensure that the rest of the test does not somehow fool itself because n1 is
	// not live.
	log.Infof(ctx, "test: resuming n1 heartbeats")
	resumeN1Heartbeats()

	// Resolve the partition, but continue blocking snapshots destined for the
	// previously-partitioned replica. The point of blocking the snapshots is to
	// prevent the respective replica from catching up and becoming eligible to
	// become the leader/leaseholder. The point of resolving the partition is to
	// allow the replica in question to figure out that it's not the leader any
	// more. As long as it is completely partitioned, the replica continues
	// believing that it is the leader, and lease acquisition requests block.
	log.Infof(ctx, "test: removing partition")
	slowSnapHandler := &slowSnapRaftHandler{
		rangeID:                    rngDesc.RangeID,
		waitCh:                     make(chan struct{}),
		IncomingRaftMessageHandler: partitionStore,
	}
	defer slowSnapHandler.unblock()
	partitionStore.Transport().ListenIncomingRaftMessages(partitionStore.Ident.StoreID, slowSnapHandler)
	// Remove the unreliable transport from the other stores, so that messages
	// sent by the partitioned store can reach them.
	for _, i := range []int{0, 1, 2} {
		if i == partitionNodeIdx {
			// We've handled the partitioned store above.
			continue
		}
		store := tc.GetFirstStoreFromServer(t, i)
		store.Transport().ListenIncomingRaftMessages(store.Ident.StoreID, store)
	}

	// Now we're going to send a request to the behind replica, and we expect it
	// to not block; we expect a redirection to the leader.
	log.Infof(ctx, "test: sending request")
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		getRequest := getArgs(key)
		_, pErr := kv.SendWrapped(timeoutCtx, partitionedStoreSender, getRequest)
		require.Error(t, pErr.GoError(), "unexpected success")
		nlhe := &kvpb.NotLeaseHolderError{}
		require.ErrorAs(t, pErr.GetDetail(), &nlhe)
		// Someone else (e.g. the Raft scheduler) may have attempted to acquire the
		// lease in the meanwhile, bumping the node's epoch and causing an
		// ErrEpochIncremented, so we ignore these and try again.
		if strings.Contains(nlhe.Error(), liveness.ErrEpochIncremented.Error()) { // no cause chain
			t.Logf("got %s, retrying", nlhe)
			continue
		}
		// When the old leader is partitioned, it will step down due to CheckQuorum.
		// Immediately after the partition is healed, the replica will not know who
		// the new leader is, so it will return a NotLeaseholderError with an empty
		// lease. This is expected and better than blocking. Still, we have the test
		// retry to make sure that the new leader is eventually discovered and that
		// after that point, the NotLeaseholderErrors start including a lease which
		// points to the new leader.
		if nlhe.Lease.Empty() {
			t.Logf("got %s, retrying", nlhe)
			continue
		}
		require.Equal(t, leaderReplicaID, nlhe.Lease.Replica.ReplicaID)
		break
	}
}

// TestSnapshotAfterTruncationWithUncommittedTail is similar in spirit to
// TestSnapshotAfterTruncation/differentTerm. However, it differs in that we
// take care to ensure that the partitioned Replica has a long uncommitted tail
// of Raft entries that is not entirely overwritten by the snapshot it receives
// after the partition heals. If the recipient of the snapshot did not purge its
// Raft entry cache when receiving the snapshot, it could get stuck repeatedly
// rejecting attempts to catch it up. This serves as a regression test for the
// bug seen in #37056.
//
// This test is only run with Epoch leases.
func TestSnapshotAfterTruncationWithUncommittedTailEpochLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	// Only run the test with epoch based leases.
	st := cluster.MakeTestingClusterSettings()
	kvserver.OverrideDefaultLeaseType(ctx, &st.SV, roachpb.LeaseEpoch)

	manualClock := hlc.NewHybridManualClock()
	tc := testcluster.StartTestCluster(t, 3,
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				Settings: st,
				Knobs: base.TestingKnobs{
					Server: &server.TestingKnobs{
						WallClock: manualClock,
					},
				},
			},
		})
	defer tc.Stopper().Stop(ctx)
	store := tc.GetFirstStoreFromServer(t, 0)

	key := tc.ScratchRangeWithExpirationLease(t)
	incA := int64(5)
	incB := int64(7)
	incC := int64(9)
	incAB := incA + incB
	incABC := incAB + incC

	// Set up a key to replicate across the cluster. We're going to modify this
	// key and truncate the raft logs from that command after partitioning one
	// of the nodes to check that it gets the new value after it reconnects.
	// We're then going to continue modifying this key to make sure that the
	// temporarily partitioned node can continue to receive updates.
	incArgs := incrementArgs(key, incA)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), incArgs); pErr != nil {
		t.Fatal(pErr)
	}

	tc.AddVotersOrFatal(t, key, tc.Targets(1, 2)...)
	require.NoError(t, tc.WaitForVoters(key, tc.Targets(1, 2)...))
	tc.WaitForValues(t, key, []int64{incA, incA, incA})

	// We partition the original leader from the other two replicas. This allows
	// us to build up a large uncommitted Raft log on the partitioned node.
	const partStore = 0
	partRepl := tc.GetFirstStoreFromServer(t, partStore).LookupReplica(roachpb.RKey(key))
	partReplDesc, err := partRepl.GetReplicaDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	partReplSender := tc.GetFirstStoreFromServer(t, partStore).TestSender()

	// Partition the original leader from its followers. We do this by installing
	// unreliableRaftHandler listeners on all three Stores. The handler on the
	// partitioned store filters out all messages while the handler on the other
	// two stores only filters out messages from the partitioned store. The
	// configuration looks like:
	//
	//           [0]
	//          x  x
	//         /    \
	//        x      x
	//      [1]<---->[2]
	//
	log.Infof(ctx, "test: installing unreliable Raft transports")
	for _, s := range []int{0, 1, 2} {
		h := &unreliableRaftHandler{
			rangeID:                    partRepl.RangeID,
			IncomingRaftMessageHandler: tc.GetFirstStoreFromServer(t, s),
		}
		if s != partStore {
			// Only filter messages from the partitioned store on the other
			// two stores.
			h.dropReq = func(req *kvserverpb.RaftMessageRequest) bool {
				return req.FromReplica.StoreID == partRepl.StoreID()
			}
			h.dropHB = func(hb *kvserverpb.RaftHeartbeat) bool {
				return hb.FromReplicaID == partReplDesc.ReplicaID
			}
		}
		tc.Servers[s].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(tc.Target(s).StoreID, h)
	}

	// Perform a series of writes on the partitioned replica. The writes will
	// not succeed before their context is canceled, but they will be appended
	// to the partitioned replica's Raft log because it is currently the Raft
	// leader.
	log.Infof(ctx, "test: sending writes to partitioned replica")
	g := ctxgroup.WithContext(ctx)
	otherKeys := make([]roachpb.Key, 32)
	otherKeys[0] = key.Next()
	for i := 1; i < 32; i++ {
		otherKeys[i] = otherKeys[i-1].Next()
	}
	for i := range otherKeys {
		// This makes the race detector happy.
		otherKey := otherKeys[i]
		g.GoCtx(func(ctx context.Context) error {
			cCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			incArgsOther := incrementArgs(otherKey, 1)
			if _, pErr := kv.SendWrapped(cCtx, partReplSender, incArgsOther); pErr == nil {
				return errors.New("unexpected success")
			} else if !testutils.IsPError(pErr, "context deadline exceeded") {
				return pErr.GoError()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	// Transfer the lease to one of the followers and perform a write. The
	// partition ensures that this will require a Raft leadership change. It's
	// unpredictable which one of the followers will become leader. Only the
	// leader will be allowed to acquire the lease (see
	// TestSnapshotAfterTruncationWithUncommittedTail), so it's also unpredictable
	// who will get the lease. We try repeatedly sending requests to both
	// candidates until one of them succeeds.
	var nonPartitionedSenders [2]kv.Sender
	nonPartitionedSenders[0] = tc.GetFirstStoreFromServer(t, 1).TestSender()
	nonPartitionedSenders[1] = tc.GetFirstStoreFromServer(t, 2).TestSender()

	log.Infof(ctx, "test: sending write to transfer lease")
	incArgs = incrementArgs(key, incB)
	var i int
	var newLeaderRepl *kvserver.Replica
	var newLeaderReplSender kv.Sender
	testutils.SucceedsSoon(t, func() error {
		manualClock.Increment(store.GetStoreConfig().LeaseExpiration())
		i++
		sender := nonPartitionedSenders[i%2]
		_, pErr := kv.SendWrapped(ctx, sender, incArgs)
		if _, ok := pErr.GetDetail().(*kvpb.NotLeaseHolderError); ok {
			return pErr.GoError()
		} else if pErr != nil {
			t.Fatal(pErr)
		}

		// A request succeeded, proving that there is a new leader and leaseholder.
		// Remember who that is.
		newLeaderStoreIdx := 1 + (i % 2)
		newLeaderRepl = tc.GetFirstStoreFromServer(t, newLeaderStoreIdx).LookupReplica(roachpb.RKey(key))
		newLeaderReplSender = tc.GetFirstStoreFromServer(t, newLeaderStoreIdx).TestSender()
		return nil
	})
	log.Infof(ctx, "test: waiting for values...")
	tc.WaitForValues(t, key, []int64{incA, incAB, incAB})
	log.Infof(ctx, "test: waiting for values... done")

	index := newLeaderRepl.GetLastIndex()

	// Truncate the log at index+1 (log entries < N are removed, so this
	// includes the increment).
	log.Infof(ctx, "test: truncating log")
	truncArgs := truncateLogArgs(index+1, partRepl.RangeID)
	truncArgs.Key = partRepl.Desc().StartKey.AsRawKey()
	testutils.SucceedsSoon(t, func() error {
		manualClock.Increment(store.GetStoreConfig().LeaseExpiration())
		_, pErr := kv.SendWrappedWith(ctx, newLeaderReplSender, kvpb.Header{RangeID: partRepl.RangeID}, truncArgs)
		if _, ok := pErr.GetDetail().(*kvpb.NotLeaseHolderError); ok {
			return pErr.GoError()
		} else if pErr != nil {
			t.Fatal(pErr)
		}
		return nil
	})
	waitForTruncationForTesting(t, newLeaderRepl, index+1)

	snapsMetric := tc.GetFirstStoreFromServer(t, partStore).Metrics().RangeSnapshotsAppliedByVoters
	snapsBefore := snapsMetric.Count()

	// Remove the partition. Snapshot should follow.
	log.Infof(ctx, "test: removing the partition")
	for _, s := range []int{0, 1, 2} {
		tc.Servers[s].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(tc.Target(s).StoreID, &unreliableRaftHandler{
			rangeID:                    partRepl.RangeID,
			IncomingRaftMessageHandler: tc.GetFirstStoreFromServer(t, s),
			unreliableRaftHandlerFuncs: unreliableRaftHandlerFuncs{
				dropReq: func(req *kvserverpb.RaftMessageRequest) bool {
					// Make sure that even going forward no MsgApp for what we just truncated can
					// make it through. The Raft transport is asynchronous so this is necessary
					// to make the test pass reliably.
					// NB: the Index on the message is the log index that _precedes_ any of the
					// entries in the MsgApp, so filter where msg.Index < index, not <= index.
					return req.Message.Type == raftpb.MsgApp && kvpb.RaftIndex(req.Message.Index) < index
				},
				dropHB:   func(*kvserverpb.RaftHeartbeat) bool { return false },
				dropResp: func(*kvserverpb.RaftMessageResponse) bool { return false },
			},
		})
	}

	// The partitioned replica should catch up after a snapshot.
	testutils.SucceedsSoon(t, func() error {
		snapsAfter := snapsMetric.Count()
		if !(snapsAfter > snapsBefore) {
			return errors.New("expected at least 1 snapshot to catch the partitioned replica up")
		}
		return nil
	})
	tc.WaitForValues(t, key, []int64{incAB, incAB, incAB})

	// Perform another write. The partitioned replica should be able to receive
	// replicated updates.
	incArgs = incrementArgs(key, incC)
	if _, pErr := kv.SendWrapped(ctx, tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
		t.Fatal(pErr)
	}
	tc.WaitForValues(t, key, []int64{incABC, incABC, incABC})
}

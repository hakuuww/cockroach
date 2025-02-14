// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import "github.com/cockroachdb/errors"

type TermCache struct {
	cache []entryID
	// lastIndex is the last index know to the TermCache that is in the raftLog
	// the entry at lastIndex has the same term as TermCache.cache's
	// last element's term
	lastIndex uint64
	maxSize   uint64
}

// ErrInvalidEntryID is returned when the supplied entryID is invalid for the operation.
var ErrInvalidEntryID = errors.New("invalid entry ID")

// ErrUnavailableInTermCache is returned when the term is not available in the cache.
// It can still be found in a lower level cache(ie. raft entry cache)
var ErrUnavailableInTermCache = errors.New("term not available")

// ErrInconsistent is returned when the term is not consistent with the raftLog
// Upon receiving this error, the caller shouldn't look further down the stack
var ErrInconsistent = errors.New("provided entry to termCache is inconsistent with raftLog")

// NewTermCache initializes a TermCache with a fixed maxSize.
func NewTermCache(size uint64) *TermCache {
	return &TermCache{
		cache:     make([]entryID, 0, size),
		maxSize:   size,
		lastIndex: 0,
	}
}

// Append adds a new entryID to the cache.
// If the cache is full, the oldest entryID is removed.
func (tc *TermCache) Append(newEntry entryID) error {
	// the entry index should be strictly increasing
	if newEntry.index <= tc.lastIndex {
		return ErrInvalidEntryID
	}

	// the entry term should be increasing
	if newEntry.term < tc.getLastEntry().term {
		return ErrInvalidEntryID
	}

	defer func() {
		// update the last entry of the cache
		tc.lastIndex = newEntry.index
	}()

	// if the term is the same as the last entry, update the last entry's index
	if newEntry.term == tc.getLastEntry().term {
	}

	// the newEntry has a higher term than the last entry

	// remove the first entry if the cache is full
	if uint64(len(tc.cache)) == tc.maxSize {
		tc.cache = tc.cache[1:]
	}

	tc.cache = append(tc.cache, newEntry)
	return nil
}

// Match returns whether the entryID is in the TermCache.
func (tc *TermCache) Match(argEntryId entryID) (bool, error) {
	if argEntryId.index < tc.cache[0].index ||
		argEntryId.index > tc.lastIndex ||
		argEntryId.term < tc.cache[0].term ||
		argEntryId.term > tc.getLastEntry().term {
		return false, ErrUnavailableInTermCache
	}

	if argEntryId.term == tc.getLastEntry().term {
		if argEntryId.index >= tc.getLastEntry().index && argEntryId.index <= tc.lastIndex {
			return true, nil
		}
		return false, ErrInconsistent
	}

	// tc.cache[i].index < tc.cache[i+1].index is equivalent to
	// tc.cache[i].index <= tc.cache[i+1].index-1
	for i := 0; i < len(tc.cache)-1; i++ {
		if argEntryId.term == tc.cache[i].term &&
			argEntryId.index >= tc.cache[i].index &&
			argEntryId.index < tc.cache[i+1].index {
			return true, nil
		}
	}

	return false, ErrInconsistent
}

func (tc *TermCache) getLastEntry() entryID {
	return tc.cache[len(tc.cache)-1]
}

// Copyright 2019 The gVisor Authors.
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

package stack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gvisor.dev/gvisor/pkg/sleep"
	"gvisor.dev/gvisor/pkg/tcpip"
)

const (
	// entryStoreSize is the default number of entries that will be generated and
	// added to the entry store. This number needs to be larger than the size of
	// the neighbor cache to give ample opportunity for verifying behavior during
	// cache overflows. Four times the size of the neighbor cache allows for
	// three complete cache overflows.
	entryStoreSize = 4 * neighborCacheSize

	// typicalLatency is the typical latency for an ARP or NDP packet to travel
	// to a router and back.
	typicalLatency = time.Millisecond

	// testEntryBroadcastAddr is a special address that indicates a packet should
	// be sent to all nodes.
	testEntryBroadcastAddr = tcpip.Address("broadcast")

	// testEntryLocalAddr is the source address of neighbor probes.
	testEntryLocalAddr = tcpip.Address("local_addr")

	// testEntryBroadcastLinkAddr is a special link address sent back to
	// multicast neighbor probes.
	testEntryBroadcastLinkAddr = tcpip.LinkAddress("mac_broadcast")
)

// entryDiffOpts returns the options passed to cmp.Diff to compare neighbor
// entries. The UpdatedAt field is ignored due to a lack of a deterministic
// method to predict the time that an event will be dispatched.
func entryDiffOpts() []cmp.Option {
	return []cmp.Option{
		cmpopts.IgnoreFields(NeighborEntry{}, "UpdatedAt"),
	}
}

// entryDiffOptsWithSort is like entryDiffOpts but also includes an option to
// sort slices of entries for cases where ordering must be ignored.
func entryDiffOptsWithSort() []cmp.Option {
	return []cmp.Option{
		cmpopts.IgnoreFields(NeighborEntry{}, "UpdatedAt"),
		cmpopts.SortSlices(func(a, b NeighborEntry) bool {
			return strings.Compare(string(a.Addr), string(b.Addr)) < 0
		}),
	}
}

func newTestNeighborCache(nudDisp NUDDispatcher, config NUDConfigurations, clock tcpip.Clock) *neighborCache {
	config.resetInvalidFields()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	c := &neighborCache{
		nic: &NIC{
			stack: &Stack{
				clock:   clock,
				nudDisp: nudDisp,
			},
			id: 1,
		},
		state: NewNUDState(config, rng),
	}
	c.mu.cache = make(map[tcpip.Address]*neighborEntry, neighborCacheSize)
	return c
}

// testEntryStore contains a set of IP to NeighborEntry mappings.
type testEntryStore struct {
	mu struct {
		sync.RWMutex

		entries map[tcpip.Address]NeighborEntry
	}
}

func toAddress(i int) tcpip.Address {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint8(1))
	binary.Write(buf, binary.BigEndian, uint8(0))
	binary.Write(buf, binary.BigEndian, uint16(i))
	return tcpip.Address(buf.String())
}

func toLinkAddress(i int) tcpip.LinkAddress {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint8(1))
	binary.Write(buf, binary.BigEndian, uint8(0))
	binary.Write(buf, binary.BigEndian, uint32(i))
	return tcpip.LinkAddress(buf.String())
}

// newTestEntryStore returns a testEntryStore pre-populated with entries.
func newTestEntryStore() *testEntryStore {
	entries := make(map[tcpip.Address]NeighborEntry)
	for i := 0; i < entryStoreSize; i++ {
		addr := toAddress(i)
		linkAddr := toLinkAddress(i)

		entries[addr] = NeighborEntry{
			Addr:      addr,
			LocalAddr: testEntryLocalAddr,
			LinkAddr:  linkAddr,
		}
	}
	store := &testEntryStore{}
	store.mu.entries = entries
	return store
}

// size returns the number of entries in the store.
func (s *testEntryStore) size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.mu.entries)
}

// entry returns the entry at index i. Returns an empty entry and false if i is
// out of bounds.
func (s *testEntryStore) entry(i int) (NeighborEntry, bool) {
	return s.entryByAddr(toAddress(i))
}

// entryByAddr returns the entry matching addr for situations when the index is
// not available. Returns an empty entry and false if no entries match addr.
func (s *testEntryStore) entryByAddr(addr tcpip.Address) (NeighborEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.mu.entries[addr]
	return entry, ok
}

// entries returns all entries in the store.
func (s *testEntryStore) entries() []NeighborEntry {
	entries := make([]NeighborEntry, 0, len(s.mu.entries))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := 0; i < entryStoreSize; i++ {
		addr := toAddress(i)
		if entry, ok := s.mu.entries[addr]; ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// set modifies the link addresses of an entry.
func (s *testEntryStore) set(i int, linkAddr tcpip.LinkAddress) {
	addr := toAddress(i)
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.mu.entries[addr]; ok {
		entry.LinkAddr = linkAddr
		s.mu.entries[addr] = entry
	}
}

// testLinkAddressResolver implements LinkAddressResolver to emulate sending a
// neighbor probe.
type testLinkAddressResolver struct {
	clock                tcpip.Clock
	neigh                *neighborCache
	entries              *testEntryStore
	delay                time.Duration
	onLinkAddressRequest func()
}

var _ LinkAddressResolver = (*testLinkAddressResolver)(nil)

func (r *testLinkAddressResolver) LinkAddressRequest(addr, localAddr tcpip.Address, linkAddr tcpip.LinkAddress, linkEP LinkEndpoint) *tcpip.Error {
	r.clock.AfterFunc(nil, r.delay, func() { r.fakeRequest(addr) })
	if f := r.onLinkAddressRequest; f != nil {
		f()
	}
	return nil
}

func (r *testLinkAddressResolver) fakeRequest(addr tcpip.Address) {
	if entry, ok := r.entries.entryByAddr(addr); ok {
		r.neigh.HandleConfirmation(addr, entry.LinkAddr, ReachabilityConfirmationFlags{
			Solicited: true,
			Override:  false,
			IsRouter:  false,
		})
	}
}

func (*testLinkAddressResolver) ResolveStaticAddress(addr tcpip.Address) (tcpip.LinkAddress, bool) {
	if addr == testEntryBroadcastAddr {
		return testEntryBroadcastLinkAddr, true
	}
	return "", false
}

func (*testLinkAddressResolver) LinkAddressProtocol() tcpip.NetworkProtocolNumber {
	return 0
}

type entryEvent struct {
	nicID    tcpip.NICID
	address  tcpip.Address
	linkAddr tcpip.LinkAddress
	state    NeighborState
}

func TestCacheGetConfig(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)

	if got, want := neigh.config(), c; got != want {
		t.Errorf("got neigh.config()=%+v, want %+v", got, want)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheSetConfig(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)

	c.BaseReachableTime = infiniteDuration
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1
	neigh.setConfig(c)

	if got, want := neigh.config(), c; got != want {
		t.Errorf("got neigh.config()=%+v, want %+v", got, want)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}

	clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != nil {
		t.Fatalf("unexpected error from neigh.entry(%s, %s, _, nil): %s", entry.Addr, entry.LocalAddr, err)
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

// TestCacheEntryNoLinkAddress verifies calling entry() without a
// LinkAddressResolver returns ErrNoLinkAddress.
func TestCacheEntryNoLinkAddress(t *testing.T) {
	nudDisp := testNUDDispatcher{}
	c := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, c, &clock)
	store := newTestEntryStore()

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, _, err := neigh.entry(entry.Addr, entry.LocalAddr, nil, nil)
	if err != tcpip.ErrNoLinkAddress {
		t.Errorf("got neigh.entry(%s, %s, nil, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrNoLinkAddress)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheRemoveEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}

	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	neigh.removeEntry(entry.Addr)
	clock.advanceAll()

	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
}

type testContext struct {
	clock   *fakeClock
	neigh   *neighborCache
	store   *testEntryStore
	linkRes *testLinkAddressResolver
	nudDisp *testNUDDispatcher
}

func newTestContext(c NUDConfigurations) testContext {
	nudDisp := &testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(nudDisp, c, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	return testContext{
		clock:   &clock,
		neigh:   neigh,
		store:   store,
		linkRes: linkRes,
		nudDisp: nudDisp,
	}
}

type overflowOptions struct {
	startAtEntryIndex int
	wantStaticEntries []NeighborEntry
}

func (c *testContext) overflowCache(opts overflowOptions) error {
	// Fill the neighbor cache to capacity to verify the LRU eviction strategy is
	// working properly after the entry removal.
	for i := opts.startAtEntryIndex; i < c.store.size(); i++ {
		// Add a new entry
		entry, ok := c.store.entry(i)
		if !ok {
			return fmt.Errorf("c.store.entry(%d) not found", i)
		}
		if _, _, err := c.neigh.entry(entry.Addr, entry.LocalAddr, c.linkRes, nil); err != tcpip.ErrWouldBlock {
			return fmt.Errorf("got c.neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
		}
		c.clock.advanceAll()

		var wantEvents []testEntryEventInfo

		// When beyond the full capacity, the cache will evict an entry as per the
		// LRU eviction strategy. Note that the number of static entries should not
		// affect the total number of dynamic entries that can be added.
		if i >= neighborCacheSize+opts.startAtEntryIndex {
			removedEntry, ok := c.store.entry(i - neighborCacheSize)
			if !ok {
				return fmt.Errorf("store.entry(%d) not found", i-neighborCacheSize)
			}
			wantEvents = append(wantEvents, testEntryEventInfo{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      removedEntry.Addr,
				LinkAddr:  removedEntry.LinkAddr,
				State:     Reachable,
			})
		}

		wantEvents = append(wantEvents, testEntryEventInfo{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		}, testEntryEventInfo{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		})

		c.nudDisp.mu.Lock()
		diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
		if diff != "" {
			return fmt.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	// Expect to find only the most recent entries. The order of entries reported
	// by entries() is undeterministic, so entries have to be sorted before
	// comparison.
	wantUnsortedEntries := opts.wantStaticEntries
	for i := c.store.size() - neighborCacheSize; i < c.store.size(); i++ {
		entry, ok := c.store.entry(i)
		if !ok {
			return fmt.Errorf("c.store.entry(%d) not found", i)
		}
		wantEntry := NeighborEntry{
			Addr:      entry.Addr,
			LocalAddr: entry.LocalAddr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		}
		wantUnsortedEntries = append(wantUnsortedEntries, wantEntry)
	}

	if diff := cmp.Diff(c.neigh.entries(), wantUnsortedEntries, entryDiffOptsWithSort()...); diff != "" {
		return fmt.Errorf("neighbor entries mismatch (-got, +want):\n%s", diff)
	}

	// No more events should have been dispatched.
	c.nudDisp.mu.Lock()
	defer c.nudDisp.mu.Unlock()
	if diff := cmp.Diff(c.nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		return fmt.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	return nil
}

// TestCacheOverflow verifies that the LRU cache eviction strategy respects the
// dynamic entry count.
func TestCacheOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	c := newTestContext(config)
	opts := overflowOptions{
		startAtEntryIndex: 0,
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

// TestCacheRemoveEntryThenOverflow verifies that the LRU cache eviction
// strategy respects the dynamic entry count when an entry is removed.
func TestCacheRemoveEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	// Add a dynamic entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	_, _, err := c.neigh.entry(entry.Addr, entry.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got c.neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Remove the entry
	c.neigh.removeEntry(entry.Addr)
	c.clock.advanceAll()
	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
		}
		c.nudDisp.mu.Lock()
		diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	opts := overflowOptions{
		startAtEntryIndex: 0,
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

// TestCacheDuplicateStaticEntryWithSameLinkAddress verifies that adding a
// duplicate static entry with the same link address does not dispatch any
// events.
func TestCacheDuplicateStaticEntryWithSameLinkAddress(t *testing.T) {
	config := DefaultNUDConfigurations()
	c := newTestContext(config)

	// Add a static entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	staticLinkAddr := entry.LinkAddr + "static"
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Remove the static entry that was just added
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()

	// No more events should have been dispatched.
	c.nudDisp.mu.Lock()
	if diff := cmp.Diff(c.nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	c.nudDisp.mu.Unlock()
}

// TestCacheDuplicateStaticEntryWithDifferentLinkAddress verifies that adding a
// duplicate static entry with a different link address dispatches a change
// event.
func TestCacheDuplicateStaticEntryWithDifferentLinkAddress(t *testing.T) {
	config := DefaultNUDConfigurations()
	c := newTestContext(config)

	// Add a static entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	staticLinkAddr := entry.LinkAddr + "static"
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Add a duplicate entry with a different link address
	staticLinkAddr += "duplicate"
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()
	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  staticLinkAddr,
				State:     Static,
			},
		}
		c.nudDisp.mu.Lock()
		if diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...); diff != "" {
			t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
		c.nudDisp.mu.Unlock()
	}
}

// TestCacheRemoveStaticEntryThenOverflow verifies that the LRU cache eviction
// strategy respects the dynamic entry count when a static entry is added then
// removed. In this case, the dynamic entry count shouldn't have been touched.
func TestCacheRemoveStaticEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	// Add a static entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	staticLinkAddr := entry.LinkAddr + "static"
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  staticLinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Remove the static entry that was just added
	c.neigh.removeEntry(entry.Addr)
	c.clock.advanceAll()
	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  staticLinkAddr,
				State:     Static,
			},
		}
		c.nudDisp.mu.Lock()
		diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	opts := overflowOptions{
		startAtEntryIndex: 0,
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

// TestCacheOverwriteWithStaticEntryThenOverflow verifies that the LRU cache
// eviction strategy keeps count of the dynamic entry count when an entry is
// overwritten by a static entry. Static entries should not count towards the
// size of the LRU cache.
func TestCacheOverwriteWithStaticEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	// Add a dynamic entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	_, _, err := c.neigh.entry(entry.Addr, entry.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got c.neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Override the entry with a static one using the same address
	staticLinkAddr := entry.LinkAddr + "static"
	c.neigh.addStaticEntry(entry.Addr, staticLinkAddr)
	c.clock.advanceAll()
	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  staticLinkAddr,
				State:     Static,
			},
		}
		c.nudDisp.mu.Lock()
		diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	opts := overflowOptions{
		startAtEntryIndex: 1,
		wantStaticEntries: []NeighborEntry{
			{
				Addr:      entry.Addr,
				LocalAddr: "", // static entries don't need a local address
				LinkAddr:  staticLinkAddr,
				State:     Static,
			},
		},
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

func TestCacheNotifiesWaker(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	w := sleep.Waker{}
	s := sleep.Sleeper{}
	const wakerID = 1
	s.AddWaker(&w, wakerID)

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, &w)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, _ = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	if doneCh == nil {
		t.Fatalf("expected done channel from neigh.entry(%s, %s, _, _)", entry.Addr, entry.LocalAddr)
	}
	clock.advanceAll()

	select {
	case <-doneCh:
	default:
		t.Fatal("expected notification from done channel")
	}

	id, ok := s.Fetch(false /* block */)
	if !ok {
		t.Errorf("expected waker to be notified after neigh.entry(%s, %s, _, _)", entry.Addr, entry.LocalAddr)
	}
	if id != wakerID {
		t.Errorf("got s.Fetch(false)=%d, want %d", id, wakerID)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheRemoveWaker(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	w := sleep.Waker{}
	s := sleep.Sleeper{}
	const wakerID = 1
	s.AddWaker(&w, wakerID)

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, &w)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, _) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	if doneCh == nil {
		t.Fatalf("expected done channel from neigh.entry(%s, %s, _, _)", entry.Addr, entry.LocalAddr)
	}

	// Remove the waker before the neighbor cache has the opportunity to send a
	// notification.
	neigh.removeWaker(entry.Addr, &w)
	clock.advanceAll()

	select {
	case <-doneCh:
	default:
		t.Fatal("expected notification from done channel")
	}

	if id, ok := s.Fetch(false /* block */); ok {
		t.Errorf("unexpected notification from waker with id=%d", id)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheAddStaticEntryThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	c.neigh.addStaticEntry(entry.Addr, entry.LinkAddr)
	e, _, err := c.neigh.entry(entry.Addr, "", nil, nil)
	if err != nil {
		t.Errorf("unexpected error from c.neigh.entry(%s, \"\", nil nil): %s", entry.Addr, err)
	}
	want := NeighborEntry{
		Addr:      entry.Addr,
		LocalAddr: "", // static entries don't need a local address
		LinkAddr:  entry.LinkAddr,
		State:     Static,
	}
	if diff := cmp.Diff(e, want, entryDiffOpts()...); diff != "" {
		t.Errorf("c.neigh.entry(%s, \"\", nil, nil) mismatch (-got, +want):\n%s", entry.Addr, diff)
	}
	c.clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	opts := overflowOptions{
		startAtEntryIndex: 1,
		wantStaticEntries: []NeighborEntry{
			{
				Addr:      entry.Addr,
				LocalAddr: "", // static entries don't need a local address
				LinkAddr:  entry.LinkAddr,
				State:     Static,
			},
		},
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

func TestCacheRemoveStaticEntry(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	c.neigh.addStaticEntry(entry.Addr, entry.LinkAddr)
	c.clock.advanceAll()
	c.neigh.removeEntry(entry.Addr)
	c.clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Static,
		},
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Static,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	opts := overflowOptions{
		startAtEntryIndex: 0,
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

func TestCacheClear(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Add a dynamic entry.
	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	nudDisp.mu.Lock()
	diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	nudDisp.mu.events = nil
	nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Add a static entry.
	neigh.addStaticEntry(entryTestAddr1, entryTestLinkAddr1)
	clock.advanceAll()

	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      entryTestAddr1,
				LinkAddr:  entryTestLinkAddr1,
				State:     Static,
			},
		}
		nudDisp.mu.Lock()
		diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	// Clear shoud remove both dynamic and static entries.
	neigh.clear()
	clock.advanceAll()

	// Remove events dispatched from clear() have no deterministic order so they
	// need to be sorted beforehand.
	wantUnsortedEvents := []testEntryEventInfo{
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
		{
			EventType: entryTestRemoved,
			NICID:     1,
			Addr:      entryTestAddr1,
			LinkAddr:  entryTestLinkAddr1,
			State:     Static,
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, wantUnsortedEvents, eventDiffOptsWithSort()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

// TestCacheClearThenOverflow verifies that the LRU cache eviction strategy
// keeps count of the dynamic entry count when all entries are cleared.
func TestCacheClearThenOverflow(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	c := newTestContext(config)

	// Add a dynamic entry
	entry, ok := c.store.entry(0)
	if !ok {
		t.Fatalf("c.store.entry(0) not found")
	}
	_, _, err := c.neigh.entry(entry.Addr, entry.LocalAddr, c.linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Errorf("got c.neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	c.clock.advanceAll()
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     1,
			Addr:      entry.Addr,
			State:     Incomplete,
		},
		{
			EventType: entryTestChanged,
			NICID:     1,
			Addr:      entry.Addr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		},
	}
	c.nudDisp.mu.Lock()
	diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
	c.nudDisp.mu.events = nil
	c.nudDisp.mu.Unlock()
	if diff != "" {
		t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}

	// Clear the cache.
	c.neigh.clear()
	c.clock.advanceAll()
	{
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
		}
		c.nudDisp.mu.Lock()
		diff := cmp.Diff(c.nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		c.nudDisp.mu.events = nil
		c.nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	opts := overflowOptions{
		startAtEntryIndex: 0,
	}
	if err := c.overflowCache(opts); err != nil {
		t.Errorf("c.overflowCache(%+v): %s", opts, err)
	}
}

func TestCacheKeepFrequentlyUsed(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	frequentlyUsedEntry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}

	// The following logic is very similar to overflowCache, but
	// periodically refreshes the frequently used entry.

	// Fill the neighbor cache to capacity
	for i := 0; i < neighborCacheSize; i++ {
		entry, ok := store.entry(i)
		if !ok {
			t.Fatalf("store.entry(%d) not found", i)
		}
		_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
		}
		clock.advanceAll()
		select {
		case <-doneCh:
		default:
			t.Fatalf("expected notification from done channel returned by neigh.entry(%s, %s, _, nil)", entry.Addr, entry.LocalAddr)
		}
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      entry.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	// Keep adding more entries
	for i := neighborCacheSize; i < store.size(); i++ {
		// Periodically refresh the frequently used entry
		if i%(neighborCacheSize/2) == 0 {
			_, _, err := neigh.entry(frequentlyUsedEntry.Addr, frequentlyUsedEntry.LocalAddr, linkRes, nil)
			if err != nil {
				t.Errorf("unexpected error from neigh.entry(%s, %s, _, nil): %s", frequentlyUsedEntry.Addr, frequentlyUsedEntry.LocalAddr, err)
			}
		}

		entry, ok := store.entry(i)
		if !ok {
			t.Fatalf("store.entry(%d) not found", i)
		}
		_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			t.Errorf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
		}
		clock.advanceAll()
		select {
		case <-doneCh:
		default:
			t.Fatalf("expected notification from done channel returned by neigh.entry(%s, %s, _, nil)", entry.Addr, entry.LocalAddr)
		}

		// An entry should have been removed, as per the LRU eviction strategy
		removedEntry, ok := store.entry(i - neighborCacheSize + 1)
		if !ok {
			t.Fatalf("store.entry(%d) not found", i-neighborCacheSize+1)
		}
		wantEvents := []testEntryEventInfo{
			{
				EventType: entryTestRemoved,
				NICID:     1,
				Addr:      removedEntry.Addr,
				LinkAddr:  removedEntry.LinkAddr,
				State:     Reachable,
			},
			{
				EventType: entryTestAdded,
				NICID:     1,
				Addr:      entry.Addr,
				State:     Incomplete,
			},
			{
				EventType: entryTestChanged,
				NICID:     1,
				Addr:      entry.Addr,
				LinkAddr:  entry.LinkAddr,
				State:     Reachable,
			},
		}
		nudDisp.mu.Lock()
		diff := cmp.Diff(nudDisp.mu.events, wantEvents, eventDiffOpts()...)
		nudDisp.mu.events = nil
		nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
		}
	}

	// Expect to find only the frequently used entry and the most recent entries.
	// The order of entries reported by entries() is undeterministic, so entries
	// have to be sorted before comparison.
	wantUnsortedEntries := []NeighborEntry{
		{
			Addr:      frequentlyUsedEntry.Addr,
			LocalAddr: frequentlyUsedEntry.LocalAddr,
			LinkAddr:  frequentlyUsedEntry.LinkAddr,
			State:     Reachable,
		},
	}

	for i := store.size() - neighborCacheSize + 1; i < store.size(); i++ {
		entry, ok := store.entry(i)
		if !ok {
			t.Fatalf("store.entry(%d) not found", i)
		}
		wantEntry := NeighborEntry{
			Addr:      entry.Addr,
			LocalAddr: entry.LocalAddr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		}
		wantUnsortedEntries = append(wantUnsortedEntries, wantEntry)
	}

	if diff := cmp.Diff(neigh.entries(), wantUnsortedEntries, entryDiffOptsWithSort()...); diff != "" {
		t.Errorf("neighbor entries mismatch (-got, +want):\n%s", diff)
	}

	// No more events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff(nudDisp.mu.events, []testEntryEventInfo(nil)); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-got, +want):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestCacheConcurrent(t *testing.T) {
	const concurrentProcesses = 16

	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	storeEntries := store.entries()
	for _, entry := range storeEntries {
		var wg sync.WaitGroup
		for r := 0; r < concurrentProcesses; r++ {
			wg.Add(1)
			go func(entry NeighborEntry) {
				defer wg.Done()
				e, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
				if err != nil && err != tcpip.ErrWouldBlock {
					t.Errorf("got neigh.entry(%s, %s, _, nil) = (%+v, _, %s), want (_, _, nil) or (_, _, %s)", entry.Addr, entry.LocalAddr, e, err, tcpip.ErrWouldBlock)
				}
			}(entry)
		}

		// Wait for all gorountines to send a request
		wg.Wait()

		// Process all the requests for a single entry concurrently
		clock.advanceAll()
	}

	// All goroutines add in the same order and add more values than can fit in
	// the cache. Our eviction strategy requires that the last entries are
	// present, up to the size of the neighbor cache, and the rest are missing.
	// The order of entries reported by entries() is undeterministic, so entries
	// have to be sorted before comparison.
	var wantUnsortedEntries []NeighborEntry
	for i := store.size() - neighborCacheSize; i < store.size(); i++ {
		entry, ok := store.entry(i)
		if !ok {
			t.Errorf("store.entry(%d) not found", i)
		}
		wantEntry := NeighborEntry{
			Addr:      entry.Addr,
			LocalAddr: entry.LocalAddr,
			LinkAddr:  entry.LinkAddr,
			State:     Reachable,
		}
		wantUnsortedEntries = append(wantUnsortedEntries, wantEntry)
	}

	if diff := cmp.Diff(neigh.entries(), wantUnsortedEntries, entryDiffOptsWithSort()...); diff != "" {
		t.Errorf("neighbor entries mismatch (-got, +want):\n%s", diff)
	}
}

func TestCacheReplace(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	// Add an entry
	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	select {
	case <-doneCh:
	default:
		t.Fatalf("expected notification from done channel returned by neigh.entry(%s, %s, _, nil)", entry.Addr, entry.LocalAddr)
	}

	// Verify the entry exists
	e, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != nil {
		t.Errorf("unexpected error from neigh.entry(%s, %s, _, nil): %s", entry.Addr, entry.LocalAddr, err)
	}
	if doneCh != nil {
		t.Errorf("unexpected done channel from neigh.entry(%s, %s, _, nil): %v", entry.Addr, entry.LocalAddr, doneCh)
	}
	if t.Failed() {
		t.FailNow()
	}
	want := NeighborEntry{
		Addr:      entry.Addr,
		LocalAddr: entry.LocalAddr,
		LinkAddr:  entry.LinkAddr,
		State:     Reachable,
	}
	if diff := cmp.Diff(e, want, entryDiffOpts()...); diff != "" {
		t.Errorf("neigh.entry(%s, %s, _, nil) mismatch (-got, +want):\n%s", entry.Addr, entry.LinkAddr, diff)
	}
	clock.advanceAll()

	// Notify of a link address change
	var updatedLinkAddr tcpip.LinkAddress
	{
		entry, ok := store.entry(1)
		if !ok {
			t.Fatalf("store.entry(1) not found")
		}
		updatedLinkAddr = entry.LinkAddr
	}
	store.set(0, updatedLinkAddr)
	neigh.HandleConfirmation(entry.Addr, updatedLinkAddr, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})

	// Requesting the entry again should start address resolution
	{
		_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
		if err != tcpip.ErrWouldBlock {
			t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
		}
		clock.advanceAll()
		select {
		case <-doneCh:
		default:
			t.Fatalf("expected notification from done channel returned by neigh.entry(%s, %s, _, nil)", entry.Addr, entry.LocalAddr)
		}
	}

	// Verify the entry's new link address
	{
		e, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
		clock.advanceAll()
		if err != nil {
			t.Errorf("unexpected error from neigh.entry(%s, %s, _, nil): %s", entry.Addr, entry.LocalAddr, err)
		}
		want = NeighborEntry{
			Addr:      entry.Addr,
			LocalAddr: entry.LocalAddr,
			LinkAddr:  updatedLinkAddr,
			State:     Reachable,
		}
		if diff := cmp.Diff(e, want, entryDiffOpts()...); diff != "" {
			t.Errorf("neigh.entry(%s, %s, _, nil) mismatch (-got, +want):\n%s", entry.Addr, entry.LocalAddr, diff)
		}
	}
}

func TestCacheResolutionFailed(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.BaseReachableTime = infiniteDuration // stay in reachable
	config.MinRandomFactor = 1
	config.MaxRandomFactor = 1
	config.UnreachableTime = infiniteDuration // stay in failed

	nudDisp := testNUDDispatcher{}
	clock := fakeClock{}
	neigh := newTestNeighborCache(&nudDisp, config, &clock)
	store := newTestEntryStore()

	var requestCount uint32
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
		onLinkAddressRequest: func() {
			atomic.AddUint32(&requestCount, 1)
		},
	}

	// First, sanity check that resolution is working
	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	got, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
	if err != nil {
		t.Fatalf("unexpected error from neigh.entry(%s, %s, _, nil): %s", entry.Addr, entry.LocalAddr, err)
	}
	want := NeighborEntry{
		Addr:      entry.Addr,
		LocalAddr: entry.LocalAddr,
		LinkAddr:  entry.LinkAddr,
		State:     Reachable,
	}
	if diff := cmp.Diff(got, want, entryDiffOpts()...); diff != "" {
		t.Errorf("neigh.entry(%s, %s, _, nil) mismatch (-got, +want):\n%s", entry.Addr, entry.LocalAddr, diff)
	}
	clock.advanceAll()

	// Verify that address resolution for an unknown address returns ErrNoLinkAddress
	before := atomic.LoadUint32(&requestCount)

	entry.Addr += "2"
	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrNoLinkAddress {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrNoLinkAddress)
	}

	maxAttempts := neigh.config().MaxUnicastProbes
	if got, want := atomic.LoadUint32(&requestCount)-before, maxAttempts; got != want {
		t.Errorf("got link address request count = %d, want = %d", got, want)
	}
}

// TestCacheResolutionTimeout simulates sending MaxMulticastProbes probes and
// not retrieving a confirmation before the duration defined by
// MaxMulticastProbes * RetransmitTimer.
func TestCacheResolutionTimeout(t *testing.T) {
	config := DefaultNUDConfigurations()
	config.RetransmitTimer = time.Millisecond // small enough to cause timeout
	config.UnreachableTime = infiniteDuration // stay in failed

	clock := fakeClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   time.Minute, // large enough to cause timeout
	}

	entry, ok := store.entry(0)
	if !ok {
		t.Fatalf("store.entry(0) not found")
	}
	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrWouldBlock {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
	}
	clock.advanceAll()
	if _, _, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil); err != tcpip.ErrNoLinkAddress {
		t.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrNoLinkAddress)
	}
}

// TestStaticResolution checks that static link addresses are resolved
// immediately and don't send resolution requests.
func TestStaticResolution(t *testing.T) {
	config := DefaultNUDConfigurations()
	clock := fakeClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   typicalLatency,
	}

	got, _, err := neigh.entry(testEntryBroadcastAddr, testEntryLocalAddr, linkRes, nil)
	if err != nil {
		t.Fatalf("unexpected error from neigh.entry(%s, %s, _, nil): %s", testEntryBroadcastAddr, testEntryLocalAddr, err)
	}
	want := NeighborEntry{
		Addr:      testEntryBroadcastAddr,
		LocalAddr: testEntryLocalAddr,
		LinkAddr:  testEntryBroadcastLinkAddr,
		State:     Static,
	}
	if diff := cmp.Diff(got, want, entryDiffOpts()...); diff != "" {
		t.Errorf("neigh.entry(%s, %s, _, nil) mismatch (-got, +want):\n%s", testEntryBroadcastAddr, testEntryLocalAddr, diff)
	}
}

func BenchmarkCacheClear(b *testing.B) {
	b.StopTimer()
	config := DefaultNUDConfigurations()
	clock := tcpip.StdClock{}
	neigh := newTestNeighborCache(nil, config, &clock)
	store := newTestEntryStore()
	linkRes := &testLinkAddressResolver{
		clock:   &clock,
		neigh:   neigh,
		entries: store,
		delay:   0,
	}

	// Clear for every possible size of the cache
	for cacheSize := 0; cacheSize < neighborCacheSize; cacheSize++ {
		// Fill the neighbor cache to capacity.
		for i := 0; i < cacheSize; i++ {
			entry, ok := store.entry(i)
			if !ok {
				b.Fatalf("store.entry(%d) not found", i)
			}
			_, doneCh, err := neigh.entry(entry.Addr, entry.LocalAddr, linkRes, nil)
			if err != tcpip.ErrWouldBlock {
				b.Fatalf("got neigh.entry(%s, %s, _, nil) = %v, want %s", entry.Addr, entry.LocalAddr, err, tcpip.ErrWouldBlock)
			}
			if doneCh != nil {
				<-doneCh
			}
		}

		b.StartTimer()
		neigh.clear()
		b.StopTimer()
	}
}

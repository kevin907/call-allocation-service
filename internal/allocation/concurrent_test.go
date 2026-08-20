package allocation

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// assertCountsMatchPinnedCalls recomputes each node's load from the call map and
// checks it against the counter the allocator maintains. It is only meaningful
// while no node has reported a non-zero figure, which is why the tests below
// register everything at zero.
func assertCountsMatchPinnedCalls(t *testing.T, r *Registry) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	pinned := make(map[string]int, len(r.nodes))
	for _, c := range r.calls {
		pinned[c.nodeID]++
	}

	for id, n := range r.nodes {
		if n.currentCalls != pinned[id] {
			t.Errorf("node %s: currentCalls = %d but %d calls are pinned to it", id, n.currentCalls, pinned[id])
		}
		if n.currentCalls < 0 {
			t.Errorf("node %s: negative currentCalls %d", id, n.currentCalls)
		}
		if n.currentCalls > n.capacity {
			t.Errorf("node %s: %d calls exceeds capacity %d", id, n.currentCalls, n.capacity)
		}
	}
}

// The affinity requirement is the one place naive code races: checking the call
// map, choosing a node and recording the placement have to be one atomic step,
// not three.
func TestAllocate_SameCallIDConcurrently(t *testing.T) {
	const goroutines = 100

	r := newTestRegistry(t)
	for _, id := range []string{"node-a", "node-b", "node-c"} {
		r.UpsertNode(report(id, "eu-west", 100, 0))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		nodeIDs = make(map[string]int)
		created int
	)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			a, isNew, err := r.Allocate("abc123", "eu-west")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Allocate: %v", err)
				return
			}
			nodeIDs[a.NodeID]++
			if isNew {
				created++
			}
		}()
	}
	wg.Wait()

	if len(nodeIDs) != 1 {
		t.Errorf("got %d distinct nodes %v, want exactly 1", len(nodeIDs), nodeIDs)
	}
	if created != 1 {
		t.Errorf("%d goroutines were told they placed the call, want exactly 1", created)
	}

	total := 0
	for _, n := range r.Snapshot() {
		total += n.CurrentCalls
	}
	if total != 1 {
		t.Errorf("the fleet holds %d calls for one callId, want 1", total)
	}
	assertCountsMatchPinnedCalls(t, r)
}

// Capacity is the other shared resource, and it is what a check-then-act
// allocator oversubscribes under load.
func TestAllocate_DistinctCallsNeverExceedCapacity(t *testing.T) {
	const (
		goroutines  = 200
		perNode     = 5
		totalSlots  = perNode * 2
		wantRefused = goroutines - totalSlots
	)

	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", perNode, 0))
	r.UpsertNode(report("node-b", "eu-west", perNode, 0))

	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		placed, refused  int
		unexpectedErrors []error
	)

	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			_, _, err := r.Allocate(fmt.Sprintf("call-%d", i), "eu-west")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				placed++
			case errors.Is(err, ErrNoCapacity):
				refused++
			default:
				unexpectedErrors = append(unexpectedErrors, err)
			}
		}()
	}
	wg.Wait()

	if len(unexpectedErrors) > 0 {
		t.Errorf("unexpected errors: %v", unexpectedErrors)
	}
	if placed != totalSlots {
		t.Errorf("placed %d calls, want exactly %d", placed, totalSlots)
	}
	if refused != wantRefused {
		t.Errorf("refused %d calls, want %d", refused, wantRefused)
	}
	assertCountsMatchPinnedCalls(t, r)
}

// Allocations and terminations interleave freely, and the counter has to survive
// every ordering of them.
func TestRegistry_ConcurrentAllocateAndTerminate(t *testing.T) {
	const calls = 300

	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 1000, 0))
	r.UpsertNode(report("node-b", "eu-west", 1000, 0))

	var wg sync.WaitGroup
	wg.Add(calls)
	for i := range calls {
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("call-%d", i)
			if _, _, err := r.Allocate(id, "eu-west"); err != nil {
				t.Errorf("Allocate(%s): %v", id, err)
				return
			}
			if i%2 == 0 {
				if err := r.Terminate(id); err != nil {
					t.Errorf("Terminate(%s): %v", id, err)
				}
			}
		}()
	}
	wg.Wait()

	assertCountsMatchPinnedCalls(t, r)

	remaining := 0
	for _, n := range r.Snapshot() {
		remaining += n.CurrentCalls
	}
	if remaining != calls/2 {
		t.Errorf("%d calls still held, want %d", remaining, calls/2)
	}
}

// Reports arrive while calls are being placed. The exercise defines no ordering
// between the two, so the resulting count is not predictable; what must hold is
// that the registry stays internally consistent and race-free.
func TestRegistry_ReportsRacingAllocations(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 1000, 0))

	var wg sync.WaitGroup

	wg.Add(100)
	for i := range 100 {
		go func() {
			defer wg.Done()
			_, _, _ = r.Allocate(fmt.Sprintf("call-%d", i), "eu-west")
		}()
	}

	wg.Add(50)
	for i := range 50 {
		go func() {
			defer wg.Done()
			r.UpsertNode(report("node-a", "eu-west", 1000, i))
			r.Snapshot()
		}()
	}

	wg.Wait()

	// The real assertion here is the race detector; the count is unpredictable by
	// design, so only the one invariant that must survive any ordering is checked.
	for _, n := range r.Snapshot() {
		if n.CurrentCalls < 0 {
			t.Errorf("node %s: negative currentCalls %d", n.ID, n.CurrentCalls)
		}
	}
}

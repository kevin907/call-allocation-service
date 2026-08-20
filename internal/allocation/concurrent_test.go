package allocation

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// assertInvariants recomputes each node's placement count from the call map and
// checks it against the counter the allocator maintains. Anything that leaves
// the two disagreeing has either double-counted a call or lost one.
func assertInvariants(t *testing.T, r *Registry) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	pinned := make(map[string]int, len(r.nodes))
	for _, c := range r.calls {
		pinned[c.nodeID]++
	}

	for id, n := range r.nodes {
		if n.placed != pinned[id] {
			t.Errorf("node %s: placed = %d but %d calls are pinned to it", id, n.placed, pinned[id])
		}
		if n.placed < 0 || n.external < 0 {
			t.Errorf("node %s: negative counters placed=%d external=%d", id, n.placed, n.external)
		}
		if n.load() > n.capacity {
			t.Errorf("node %s: load %d exceeds capacity %d", id, n.load(), n.capacity)
		}
	}
}

// The affinity requirement is the one place naive code races: checking the call
// map, choosing a node and recording the placement have to be one atomic step,
// not three.
func TestAllocate_SameCallIDConcurrently(t *testing.T) {
	const goroutines = 100

	r, _ := newTestRegistry(t)
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
		total += n.PlacedCalls
	}
	if total != 1 {
		t.Errorf("the fleet holds %d placements for one call, want 1", total)
	}
	assertInvariants(t, r)
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

	r, _ := newTestRegistry(t)
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
	assertInvariants(t, r)
}

// Heartbeats arrive while calls are being placed and ended. The rebase reads
// placed, so a report racing a placement is the interleaving most likely to
// corrupt the accounting.
func TestRegistry_MixedWorkloadKeepsAccountingHonest(t *testing.T) {
	const calls = 300

	r, _ := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 1000, 0))
	r.UpsertNode(report("node-b", "eu-west", 1000, 0))

	var wg sync.WaitGroup

	wg.Add(calls)
	for i := range calls {
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("call-%d", i)
			if _, _, err := r.Allocate(id, "eu-west"); err != nil {
				return
			}
			if i%2 == 0 {
				if err := r.Terminate(id); err != nil {
					t.Errorf("Terminate(%s): %v", id, err)
				}
			}
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
	assertInvariants(t, r)

	// Half the calls terminated, so half remain pinned.
	remaining := 0
	for _, n := range r.Snapshot() {
		remaining += n.PlacedCalls
	}
	if remaining != calls/2 {
		t.Errorf("%d calls still pinned, want %d", remaining, calls/2)
	}
}

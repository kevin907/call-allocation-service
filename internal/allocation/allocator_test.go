package allocation

import (
	"errors"
	"fmt"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return New()
}

func report(id, region string, capacity, current int) Report {
	return Report{ID: id, Region: region, Capacity: capacity, CurrentCalls: current}
}

// nodeState reads a node's counters through the same view the API exposes.
func nodeState(t *testing.T, r *Registry, id string) NodeStatus {
	t.Helper()
	n, ok := r.Node(id)
	if !ok {
		t.Fatalf("node %q not registered", id)
	}
	return n
}

func mustAllocate(t *testing.T, r *Registry, callID, region string) string {
	t.Helper()
	a, _, err := r.Allocate(callID, region)
	if err != nil {
		t.Fatalf("Allocate(%q, %q): %v", callID, region, err)
	}
	return a.NodeID
}

func TestAllocate_PicksMostAvailableNode(t *testing.T) {
	r := newTestRegistry(t)
	// Utilisation and least-loaded both pick node-c on the first call; only
	// absolute headroom picks node-a.
	r.UpsertNode(report("node-a", "eu-west", 1000, 900))
	r.UpsertNode(report("node-b", "eu-west", 100, 50))
	r.UpsertNode(report("node-c", "eu-west", 4, 0))

	if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-a" {
		t.Errorf("got %q, want node-a: 100 free against 50 and 4", got)
	}
}

func TestAllocate_IgnoresOtherRegions(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-us", "us-east", 1000, 0))
	r.UpsertNode(report("node-eu", "eu-west", 10, 0))

	if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-eu" {
		t.Errorf("got %q, want node-eu despite node-us having far more room", got)
	}

	if _, _, err := r.Allocate("call-2", "ap-south"); !errors.Is(err, ErrNoNodesInRegion) {
		t.Errorf("unknown region: got %v, want ErrNoNodesInRegion", err)
	}
}

func TestAllocate_TieBreakIsDeterministic(t *testing.T) {
	// Go randomises map iteration, so a single pass proves nothing.
	for range 100 {
		r := newTestRegistry(t)
		r.UpsertNode(report("node-c", "eu-west", 100, 0))
		r.UpsertNode(report("node-a", "eu-west", 100, 0))
		r.UpsertNode(report("node-b", "eu-west", 100, 0))

		if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-a" {
			t.Fatalf("got %q, want node-a on every run", got)
		}
	}
}

// A report replaces the node's figure, and allocations adjust it locally until
// the next one. Without the local adjustment every call in a reporting interval
// would see the same snapshot and pick the same winner.
func TestUpsertNode_ReportReplacesAndAllocationsAdjust(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-1", "eu-west", 100, 20))

	if got := nodeState(t, r, "node-1"); got.CurrentCalls != 20 || got.Available != 80 {
		t.Fatalf("after a report: currentCalls=%d available=%d, want 20/80", got.CurrentCalls, got.Available)
	}

	for i := range 5 {
		mustAllocate(t, r, fmt.Sprintf("call-%d", i), "eu-west")
	}
	if got := nodeState(t, r, "node-1"); got.CurrentCalls != 25 || got.Available != 75 {
		t.Errorf("after five placements: currentCalls=%d available=%d, want 25/75", got.CurrentCalls, got.Available)
	}

	if err := r.Terminate("call-0"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := nodeState(t, r, "node-1"); got.CurrentCalls != 24 {
		t.Errorf("after a termination: currentCalls=%d, want 24", got.CurrentCalls)
	}

	// The node is the authority on its own load, so its next word wins.
	r.UpsertNode(report("node-1", "eu-west", 100, 40))
	if got := nodeState(t, r, "node-1"); got.CurrentCalls != 40 {
		t.Errorf("after a fresh report: currentCalls=%d, want 40", got.CurrentCalls)
	}
}

func TestAllocate_SpreadsLoadBetweenReports(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 100, 0))
	r.UpsertNode(report("node-b", "eu-west", 100, 0))

	// No node reports again during this burst, so a policy trusting only the last
	// snapshot would send all 100 calls to whichever node it picked first.
	for i := range 100 {
		mustAllocate(t, r, fmt.Sprintf("call-%d", i), "eu-west")
	}

	a, b := nodeState(t, r, "node-a"), nodeState(t, r, "node-b")
	if a.CurrentCalls != 50 || b.CurrentCalls != 50 {
		t.Errorf("currentCalls a=%d b=%d, want an even 50/50 split", a.CurrentCalls, b.CurrentCalls)
	}
}

func TestAllocate_AffinityReturnsTheSameNode(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 100, 0))
	r.UpsertNode(report("node-b", "eu-west", 100, 0))

	first, created, err := r.Allocate("abc123", "eu-west")
	if err != nil || !created {
		t.Fatalf("first allocate: created=%v err=%v", created, err)
	}

	for range 5 {
		again, created, err := r.Allocate("abc123", "eu-west")
		if err != nil {
			t.Fatalf("repeat allocate: %v", err)
		}
		if created {
			t.Error("a repeat allocation must not report a new placement")
		}
		if again.NodeID != first.NodeID {
			t.Fatalf("got %q, want %q", again.NodeID, first.NodeID)
		}
	}

	if got := nodeState(t, r, first.NodeID); got.CurrentCalls != 1 {
		t.Errorf("currentCalls = %d, want 1: repeats must not consume capacity", got.CurrentCalls)
	}
}

// "It must always return the same node while the call remains active" admits no
// exception, so a caller naming a different region still gets the pinned node.
func TestAllocate_AffinityOutlivesARegionChange(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-eu", "eu-west", 100, 0))
	r.UpsertNode(report("node-us", "us-east", 100, 0))
	mustAllocate(t, r, "abc123", "eu-west")

	got, created, err := r.Allocate("abc123", "us-east")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if created {
		t.Error("re-allocating an active call must not report a new placement")
	}
	if got.NodeID != "node-eu" {
		t.Errorf("nodeId = %q, want node-eu", got.NodeID)
	}
	if got.Region != "eu-west" {
		t.Errorf("region = %q, want the pinned eu-west", got.Region)
	}
	if n := nodeState(t, r, "node-us"); n.CurrentCalls != 0 {
		t.Errorf("no capacity may be taken in the requested region: currentCalls = %d", n.CurrentCalls)
	}
	if n := nodeState(t, r, "node-eu"); n.CurrentCalls != 1 {
		t.Errorf("the pinned node still holds exactly one call, got %d", n.CurrentCalls)
	}
}

func TestAllocate_RejectsWhenRegionIsFull(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 2, 0))

	mustAllocate(t, r, "call-1", "eu-west")
	mustAllocate(t, r, "call-2", "eu-west")

	if _, _, err := r.Allocate("call-3", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("got %v, want ErrNoCapacity", err)
	}

	// Reported load counts against capacity just as our own placements do.
	r.UpsertNode(report("node-b", "eu-west", 10, 10))
	if _, _, err := r.Allocate("call-3", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("a node full of external calls must not be selected: %v", err)
	}
}

func TestAllocate_ZeroCapacityNodeIsDrained(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 0, 0))

	if _, _, err := r.Allocate("call-1", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("got %v, want ErrNoCapacity: capacity 0 means drained, not deregistered", err)
	}
}

func TestUpsertNode_CapacityCutBelowLiveCalls(t *testing.T) {
	r := newTestRegistry(t)
	// A real state, not an error: the node is telling us it shrank under load.
	r.UpsertNode(report("node-a", "eu-west", 10, 25))

	got := nodeState(t, r, "node-a")
	if got.Available != -15 {
		t.Errorf("available = %d, want -15", got.Available)
	}
	if _, _, err := r.Allocate("call-1", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("got %v, want ErrNoCapacity", err)
	}
}

func TestTerminate_ReleasesCapacityExactlyOnce(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 1, 0))
	mustAllocate(t, r, "call-1", "eu-west")

	if _, _, err := r.Allocate("call-2", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("setup: got %v, want the node to be full", err)
	}
	if err := r.Terminate("call-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := mustAllocate(t, r, "call-2", "eu-west"); got != "node-a" {
		t.Errorf("got %q, want the freed slot reused", got)
	}

	if err := r.Terminate("call-1"); !errors.Is(err, ErrCallNotFound) {
		t.Errorf("second terminate: got %v, want ErrCallNotFound", err)
	}
	if got := nodeState(t, r, "node-a"); got.CurrentCalls != 1 {
		t.Errorf("currentCalls = %d, want 1: a repeated terminate must not free a slot twice", got.CurrentCalls)
	}
}

func TestTerminate_UnknownCall(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Terminate("never-existed"); !errors.Is(err, ErrCallNotFound) {
		t.Errorf("got %v, want ErrCallNotFound", err)
	}
}

func TestGet_ReturnsThePlacement(t *testing.T) {
	r := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 10, 0))
	mustAllocate(t, r, "abc123", "eu-west")

	got, err := r.Get("abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CallID != "abc123" || got.NodeID != "node-a" || got.Region != "eu-west" {
		t.Errorf("got %+v", got)
	}
	if _, err := r.Get("missing"); !errors.Is(err, ErrCallNotFound) {
		t.Errorf("got %v, want ErrCallNotFound", err)
	}
}

func TestSnapshot_EmptyRegistryIsNotNil(t *testing.T) {
	r := newTestRegistry(t)
	// A nil slice marshals to null, which is the first thing a reviewer would see
	// from GET /nodes on a freshly started service.
	if got := r.Snapshot(); got == nil {
		t.Fatal("Snapshot returned nil, want an empty slice")
	}
}

func TestUpsertNode_ReportsCreation(t *testing.T) {
	r := newTestRegistry(t)
	if _, created := r.UpsertNode(report("node-a", "eu-west", 10, 0)); !created {
		t.Error("first registration should report created")
	}
	got, created := r.UpsertNode(report("node-a", "eu-west", 20, 0))
	if created {
		t.Error("a refresh should not report created")
	}
	if got.Capacity != 20 {
		t.Errorf("capacity = %d, want the refreshed 20", got.Capacity)
	}
	if _, ok := r.Node("nope"); ok {
		t.Error("Node reported an unregistered id as present")
	}
}

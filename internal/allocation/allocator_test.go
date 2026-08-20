package allocation

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const testTTL = 30 * time.Second

// fakeClock is mutex-guarded because the concurrency tests read it from many
// goroutines while the test body advances it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clk := newClock()
	return NewWithClock(testTTL, clk.now), clk
}

func report(id, region string, capacity, current int) Report {
	return Report{ID: id, Region: region, Capacity: capacity, CurrentCalls: current}
}

// nodeState reads a node's counters through the same view the API exposes.
func nodeState(t *testing.T, r *Registry, id string) NodeStatus {
	t.Helper()
	for _, n := range r.Snapshot() {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not registered", id)
	return NodeStatus{}
}

func mustAllocate(t *testing.T, r *Registry, callID, region string) string {
	t.Helper()
	a, _, err := r.Allocate(callID, region)
	if err != nil {
		t.Fatalf("Allocate(%q, %q): %v", callID, region, err)
	}
	return a.NodeID
}

func TestUpsertNode_ReportRebasesExternalLoad(t *testing.T) {
	tests := []struct {
		name         string
		placed       int
		reported     int
		wantExternal int
		wantLoad     int
	}{
		{"report matches our own count", 25, 25, 0, 25},
		{"report lags our placements", 25, 20, 0, 25},
		{"report includes load from elsewhere", 25, 45, 20, 45},
		{"nothing placed yet", 0, 20, 20, 20},
		{"idle node", 0, 0, 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRegistry(t)
			r.UpsertNode(report("node-1", "eu-west", 100, 0))
			for i := range tc.placed {
				mustAllocate(t, r, fmt.Sprintf("call-%d", i), "eu-west")
			}

			r.UpsertNode(report("node-1", "eu-west", 100, tc.reported))

			got := nodeState(t, r, "node-1")
			if got.ExternalCalls != tc.wantExternal || got.Load != tc.wantLoad {
				t.Errorf("external=%d load=%d, want external=%d load=%d",
					got.ExternalCalls, got.Load, tc.wantExternal, tc.wantLoad)
			}
			if got.PlacedCalls != tc.placed {
				t.Errorf("a report must never disturb placed: got %d, want %d", got.PlacedCalls, tc.placed)
			}
		})
	}
}

// The rebase and the tempting max(reported, placed) agree at the instant of a
// report and diverge immediately after, which is the whole reason for the extra
// counter.
func TestUpsertNode_PlacementsAfterAReportStillCount(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.UpsertNode(report("node-1", "eu-west", 100, 0))
	for i := range 25 {
		mustAllocate(t, r, fmt.Sprintf("ours-%d", i), "eu-west")
	}
	r.UpsertNode(report("node-1", "eu-west", 100, 45)) // 20 calls arrived from elsewhere

	for i := range 5 {
		mustAllocate(t, r, fmt.Sprintf("after-%d", i), "eu-west")
	}

	got := nodeState(t, r, "node-1")
	if got.Load != 50 {
		t.Errorf("load = %d, want 50: max(reported,placed) would give 45 and lose the five new calls", got.Load)
	}
	if got.Available != 50 {
		t.Errorf("available = %d, want 50", got.Available)
	}
}

func TestAllocate_PicksMostAvailableNode(t *testing.T) {
	r, _ := newTestRegistry(t)
	// Utilisation ordering would pick node-c and saturate four slots while 150
	// sit idle elsewhere.
	r.UpsertNode(report("node-a", "eu-west", 1000, 900))
	r.UpsertNode(report("node-b", "eu-west", 100, 50))
	r.UpsertNode(report("node-c", "eu-west", 4, 0))

	if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-a" {
		t.Errorf("got %q, want node-a: 100 free against 50 and 4", got)
	}
}

func TestAllocate_IgnoresOtherRegions(t *testing.T) {
	r, _ := newTestRegistry(t)
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
		r, _ := newTestRegistry(t)
		r.UpsertNode(report("node-c", "eu-west", 100, 0))
		r.UpsertNode(report("node-a", "eu-west", 100, 0))
		r.UpsertNode(report("node-b", "eu-west", 100, 0))

		if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-a" {
			t.Fatalf("got %q, want node-a on every run", got)
		}
	}
}

func TestAllocate_SpreadsLoadBetweenReports(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 100, 0))
	r.UpsertNode(report("node-b", "eu-west", 100, 0))

	// No node reports again during this burst, so a policy trusting only the last
	// snapshot would send all 100 calls to whichever node it picked first.
	for i := range 100 {
		mustAllocate(t, r, fmt.Sprintf("call-%d", i), "eu-west")
	}

	a, b := nodeState(t, r, "node-a"), nodeState(t, r, "node-b")
	if a.PlacedCalls != 50 || b.PlacedCalls != 50 {
		t.Errorf("placed a=%d b=%d, want an even 50/50 split", a.PlacedCalls, b.PlacedCalls)
	}
}

func TestAllocate_AffinityReturnsTheSameNode(t *testing.T) {
	r, _ := newTestRegistry(t)
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

	if got := nodeState(t, r, first.NodeID); got.PlacedCalls != 1 {
		t.Errorf("placed = %d, want 1: repeats must not consume capacity", got.PlacedCalls)
	}
}

func TestAllocate_AffinityOutlivesNodeHealth(t *testing.T) {
	r, clk := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 1, 0))
	pinned := mustAllocate(t, r, "abc123", "eu-west")

	clk.advance(testTTL + time.Second) // node-a stops reporting

	if got := mustAllocate(t, r, "abc123", "eu-west"); got != pinned {
		t.Errorf("got %q, want the pinned %q: media already flowing cannot be re-homed", got, pinned)
	}
}

// "It must always return the same node while the call remains active" admits no
// exception, so a caller naming a different region still gets the pinned node.
func TestAllocate_AffinityOutlivesARegionChange(t *testing.T) {
	r, _ := newTestRegistry(t)
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
	if placed := nodeState(t, r, "node-us"); placed.PlacedCalls != 0 {
		t.Errorf("no capacity may be taken in the requested region: placed = %d", placed.PlacedCalls)
	}
	if placed := nodeState(t, r, "node-eu"); placed.PlacedCalls != 1 {
		t.Errorf("the pinned node still holds exactly one call, got %d", placed.PlacedCalls)
	}
}

func TestAllocate_RejectsWhenRegionIsFull(t *testing.T) {
	r, _ := newTestRegistry(t)
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

func TestAllocate_SkipsStaleNodes(t *testing.T) {
	r, clk := newTestRegistry(t)
	r.UpsertNode(report("node-quiet", "eu-west", 100, 0))
	r.UpsertNode(report("node-live", "eu-west", 10, 0))

	clk.advance(testTTL + time.Second)
	r.UpsertNode(report("node-live", "eu-west", 10, 0)) // only this one reports again

	if got := mustAllocate(t, r, "call-1", "eu-west"); got != "node-live" {
		t.Errorf("got %q, want node-live: a node that stopped reporting is not a placement target", got)
	}
	if !nodeState(t, r, "node-quiet").Stale {
		t.Error("node-quiet should be reported stale")
	}

	// A node is excluded, never forgotten, so it recovers on its next report.
	r.UpsertNode(report("node-quiet", "eu-west", 100, 0))
	if got := mustAllocate(t, r, "call-2", "eu-west"); got != "node-quiet" {
		t.Errorf("got %q, want node-quiet back in service", got)
	}
}

func TestAllocate_ZeroCapacityNodeIsDrained(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.UpsertNode(report("node-a", "eu-west", 0, 0))

	if _, _, err := r.Allocate("call-1", "eu-west"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("got %v, want ErrNoCapacity: capacity 0 means drained, not deregistered", err)
	}
}

func TestUpsertNode_CapacityCutBelowLiveCalls(t *testing.T) {
	r, _ := newTestRegistry(t)
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
	r, _ := newTestRegistry(t)
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
	if got := nodeState(t, r, "node-a"); got.PlacedCalls != 1 {
		t.Errorf("placed = %d, want 1: a repeated terminate must not free a slot twice", got.PlacedCalls)
	}
}

func TestTerminate_UnknownCall(t *testing.T) {
	r, _ := newTestRegistry(t)
	if err := r.Terminate("never-existed"); !errors.Is(err, ErrCallNotFound) {
		t.Errorf("got %v, want ErrCallNotFound", err)
	}
}

func TestGet_ReturnsThePlacement(t *testing.T) {
	r, _ := newTestRegistry(t)
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
	r, _ := newTestRegistry(t)
	// A nil slice marshals to null, which is the first thing a reviewer would see
	// from GET /nodes on a freshly started service.
	if got := r.Snapshot(); got == nil {
		t.Fatal("Snapshot returned nil, want an empty slice")
	}
}

func TestUpsertNode_ReportsCreation(t *testing.T) {
	r, _ := newTestRegistry(t)
	if created := r.UpsertNode(report("node-a", "eu-west", 10, 0)); !created {
		t.Error("first registration should report created")
	}
	if created := r.UpsertNode(report("node-a", "eu-west", 20, 0)); created {
		t.Error("a refresh should not report created")
	}
	if got := nodeState(t, r, "node-a"); got.Capacity != 20 {
		t.Errorf("capacity = %d, want the refreshed 20", got.Capacity)
	}
}

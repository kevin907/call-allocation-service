// Package allocation owns the fleet state and the policy that places calls onto
// conferencing nodes. It holds everything in memory, is safe for concurrent use,
// and imports nothing outside the standard library. It knows no HTTP.
package allocation

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Report is a node's periodic statement of its own capacity and load.
type Report struct {
	ID           string
	Region       string
	Capacity     int
	CurrentCalls int
}

type node struct {
	id       string
	region   string
	capacity int
	reported int // the node's own last currentCalls figure
	placed   int // calls we pinned here and have not yet terminated
	external int // load we did not place, rebased at every report
	lastSeen time.Time
}

func (n *node) load() int      { return n.placed + n.external }
func (n *node) available() int { return n.capacity - n.load() }

type call struct {
	id          string
	nodeID      string
	region      string
	allocatedAt time.Time
}

func (c *call) allocation() Allocation {
	return Allocation{CallID: c.id, NodeID: c.nodeID, Region: c.region, AllocatedAt: c.allocatedAt}
}

// Allocation is a call's placement, copied out so no pointer into the registry escapes.
type Allocation struct {
	CallID      string
	NodeID      string
	Region      string
	AllocatedAt time.Time
}

// NodeStatus is the operator's view of one node. Both raw counts are exposed
// alongside the derived ones so the reconciliation can be checked by eye.
type NodeStatus struct {
	ID            string    `json:"id"`
	Region        string    `json:"region"`
	Capacity      int       `json:"capacity"`
	ReportedCalls int       `json:"reportedCalls"`
	PlacedCalls   int       `json:"placedCalls"`
	ExternalCalls int       `json:"externalCalls"`
	Load          int       `json:"load"`
	Available     int       `json:"available"`
	LastSeen      time.Time `json:"lastSeen"`
	Stale         bool      `json:"stale"`
}

// Registry holds the whole service state. Every method takes mu for its entire
// body: allocation is a read-modify-write, so there is no read-mostly path that
// would justify an RWMutex, and splitting the critical section is how affinity
// races get in.
type Registry struct {
	mu    sync.Mutex
	nodes map[string]*node
	calls map[string]*call
	ttl   time.Duration
	now   func() time.Time
}

func New(ttl time.Duration) *Registry {
	return NewWithClock(ttl, time.Now)
}

// NewWithClock lets tests advance time by hand rather than sleeping.
func NewWithClock(ttl time.Duration, now func() time.Time) *Registry {
	return &Registry{
		nodes: make(map[string]*node),
		calls: make(map[string]*call),
		ttl:   ttl,
		now:   now,
	}
}

// UpsertNode applies a capacity report, creating the node if it is unknown, and
// reports whether it was created.
func (r *Registry) UpsertNode(rep Report) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, found := r.nodes[rep.ID]
	if !found {
		n = &node{id: rep.ID}
		r.nodes[rep.ID] = n
	}

	n.region = rep.Region
	n.capacity = rep.Capacity
	n.reported = rep.CurrentCalls
	// A report is a snapshot that may predate calls we have only just placed, so
	// we keep our own count and treat the surplus as load from somewhere else.
	// Rebasing rather than accumulating is what keeps the figure honest between
	// reports; see DESIGN.md.
	n.external = max(0, rep.CurrentCalls-n.placed)
	n.lastSeen = r.now()

	return !found
}

// Terminate ends an active call and returns its slot to the node.
func (r *Registry) Terminate(callID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.calls[callID]
	if !ok {
		return ErrCallNotFound
	}
	delete(r.calls, callID)

	// The node may have vanished since the call was placed, so only give back a
	// slot we can still see.
	if n, ok := r.nodes[c.nodeID]; ok && n.placed > 0 {
		n.placed--
	}
	return nil
}

// Get returns an active call's placement.
func (r *Registry) Get(callID string) (Allocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.calls[callID]
	if !ok {
		return Allocation{}, ErrCallNotFound
	}
	return c.allocation(), nil
}

// Snapshot returns the fleet ordered by id.
func (r *Registry) Snapshot() []NodeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]NodeStatus, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, r.statusLocked(n))
	}
	slices.SortFunc(out, func(a, b NodeStatus) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Node returns one node's status.
func (r *Registry) Node(id string) (NodeStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, ok := r.nodes[id]
	if !ok {
		return NodeStatus{}, false
	}
	return r.statusLocked(n), true
}

// NodeCount reports how many nodes have registered, for the readiness endpoint.
func (r *Registry) NodeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nodes)
}

func (r *Registry) statusLocked(n *node) NodeStatus {
	return NodeStatus{
		ID:            n.id,
		Region:        n.region,
		Capacity:      n.capacity,
		ReportedCalls: n.reported,
		PlacedCalls:   n.placed,
		ExternalCalls: n.external,
		Load:          n.load(),
		Available:     n.available(),
		LastSeen:      n.lastSeen,
		Stale:         r.staleLocked(n),
	}
}

func (r *Registry) staleLocked(n *node) bool {
	return r.now().Sub(n.lastSeen) > r.ttl
}

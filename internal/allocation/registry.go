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
	id           string
	region       string
	capacity     int
	currentCalls int
	lastSeen     time.Time
}

func (n *node) available() int { return n.capacity - n.currentCalls }

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

// NodeStatus is the operator's view of one node. LastSeen is reported but never
// acted on; see DESIGN.md on node expiry.
type NodeStatus struct {
	ID           string    `json:"id"`
	Region       string    `json:"region"`
	Capacity     int       `json:"capacity"`
	CurrentCalls int       `json:"currentCalls"`
	Available    int       `json:"available"`
	LastSeen     time.Time `json:"lastSeen"`
}

// Registry holds the whole service state. One mutex covers both maps: allocation
// has to hold it across the affinity lookup, the selection and the reservation,
// because those are one state transition rather than three. An RWMutex would be
// correct too, but buys little when the main operation is a write anyway.
type Registry struct {
	mu    sync.Mutex
	nodes map[string]*node
	calls map[string]*call
}

func New() *Registry {
	return &Registry{
		nodes: make(map[string]*node),
		calls: make(map[string]*call),
	}
}

// UpsertNode applies a capacity report, creating the node if it is unknown, and
// reports whether it was created. The reported figure replaces whatever we were
// holding, because the node is the authority on its own load; allocations made
// since then adjust it locally until the next report arrives.
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
	n.currentCalls = rep.CurrentCalls
	n.lastSeen = time.Now()

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
	if n, ok := r.nodes[c.nodeID]; ok && n.currentCalls > 0 {
		n.currentCalls--
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

func (n *node) status() NodeStatus {
	return NodeStatus{
		ID:           n.id,
		Region:       n.region,
		Capacity:     n.capacity,
		CurrentCalls: n.currentCalls,
		Available:    n.available(),
		LastSeen:     n.lastSeen,
	}
}

// Snapshot returns the fleet ordered by id. The sort happens after the lock is
// released, since it only touches the copy.
func (r *Registry) Snapshot() []NodeStatus {
	r.mu.Lock()
	out := make([]NodeStatus, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n.status())
	}
	r.mu.Unlock()

	slices.SortFunc(out, func(a, b NodeStatus) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Node returns one node's status. Registration calls this on every report, so it
// is a map lookup rather than a walk of the whole fleet.
func (r *Registry) Node(id string) (NodeStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, ok := r.nodes[id]
	if !ok {
		return NodeStatus{}, false
	}
	return n.status(), true
}

// NodeCount reports how many nodes have registered, for the readiness endpoint.
func (r *Registry) NodeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nodes)
}

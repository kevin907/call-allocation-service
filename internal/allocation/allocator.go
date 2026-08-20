package allocation

import "errors"

var (
	// ErrNoNodesInRegion covers both an unknown region and one whose nodes have
	// all stopped reporting. Regions are discovered from registrations rather
	// than configured, so the service cannot honestly tell a typo from a fleet
	// that is entirely down.
	ErrNoNodesInRegion = errors.New("no healthy node registered in region")
	ErrNoCapacity      = errors.New("no node with available capacity in region")
	ErrCallNotFound    = errors.New("call not found")
)

// Allocate places a call on a node in the requested region, or returns the node
// it is already pinned to. It reports whether a new placement was made.
func (r *Registry) Allocate(callID, region string) (Allocation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Affinity is resolved before any health or capacity filter, and before the
	// requested region is even consulted. An active call keeps its node once that
	// node is full, once it has gone quiet, and even if the caller now names a
	// different region: media already flowing cannot be re-homed by editing a
	// map, and the brief admits no exception to returning the same node.
	if c, ok := r.calls[callID]; ok {
		return c.allocation(), false, nil
	}

	n, err := r.pickLocked(region)
	if err != nil {
		return Allocation{}, false, err
	}

	c := &call{id: callID, nodeID: n.id, region: region, allocatedAt: r.now()}
	r.calls[callID] = c
	n.placed++

	return c.allocation(), true, nil
}

// pickLocked chooses the healthy in-region node with the most free capacity. The
// healthy counter is what lets one pass tell "nothing to allocate to" apart from
// "everything is full", which are different answers for the caller.
func (r *Registry) pickLocked(region string) (*node, error) {
	var best *node
	healthy := 0

	for _, n := range r.nodes {
		if n.region != region || r.staleLocked(n) {
			continue
		}
		healthy++
		if n.available() > 0 && better(n, best) {
			best = n
		}
	}

	switch {
	case healthy == 0:
		return nil, ErrNoNodesInRegion
	case best == nil:
		return nil, ErrNoCapacity
	}
	return best, nil
}

// better ranks by absolute free capacity, then by id. Ranking by free slots
// rather than by utilisation stops a small node being saturated while a large
// one still has room. The id tie-break is deliberate: Go randomises map order,
// so without it the choice would be arbitrary by accident rather than by design.
func better(a, b *node) bool {
	if b == nil {
		return true
	}
	if av, bv := a.available(), b.available(); av != bv {
		return av > bv
	}
	return a.id < b.id
}

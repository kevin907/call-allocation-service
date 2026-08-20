# Design note

## Allocation

Within the requested region a call goes to the healthy node with the most **absolute free
capacity** (`capacity - load`), ties broken on lowest node id, so selection is deterministic
and testable. Full nodes are ineligible: media capacity is a hard limit, not a queue.

Headroom beats utilisation ratio — given nodes of 1000/900, 100/50 and 4/0, ratio ordering
saturates the four-slot node while 150 slots sit idle. Ratio would be safer only if capacity were
a soft estimate.

## Counting load

Nodes report `currentCalls` periodically, but we also know what we just placed there. Trusting the
report alone stampedes one node: every call between two heartbeats sees the same snapshot and
picks the same winner.

So each node keeps `reported`, `placed` (ours) and `external`; every report rebases
`external = max(0, reported - placed)`, leaving `placed` alone. `max(reported, placed)` agrees
only at the instant of a report — with reported 45 and placed 25, five further placements
give 50 under the rebase but 45 under the max. **Limitation:** this converges within one report
interval rather than being exact.

## Affinity and lifecycle

A call's node is recorded at placement and returned on every repeat, making allocation safely
retryable after a timeout. Affinity resolves before any health, capacity or region filter: flowing
media cannot be re-homed by editing a map. **Trade-off:** so a live `callId` re-allocated into a
*different* region still returns its pinned node, with the mismatch logged rather than raised. A
`409` would serve the caller better but would break a stated requirement to satisfy an inferred
one. Termination frees the slot; an unknown call is `404`, though a retry-prone caller would prefer
`204`. Nodes silent for `NODE_TTL` (30s) are excluded from new allocations but retained.

## One instance

State dies with the process, so the Deployment uses `Recreate`: at one replica a rolling update
still rounds `maxSurge` up to one pod, and two instances with disjoint call tables would briefly
share one Service. Liveness is deliberately slack — a restart destroys every mapping — while
readiness never gates on node count, since nodes register through the Service it controls.

**Deliberately out of scope:** multiple replicas (affinity would need shared state), persistence,
reaping abandoned calls (the map grows unbounded), cross-region overflow, node deregistration
(`capacity: 0` drains), auth and TLS, HPA and PodDisruptionBudget, metrics.

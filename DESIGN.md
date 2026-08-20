# Design note

## Allocation

Within the requested region a call goes to the healthy node with the most **absolute free
capacity** (`capacity - load`), ties broken on lowest node id, so selection is deterministic
and testable. Full nodes are ineligible: capacity is a hard limit, not a queue.

Headroom beats utilisation ratio — given nodes of 1000/900, 100/50 and 4/0, ratio ordering
saturates the four-slot node while 150 slots sit idle. Ratio would be safer only if capacity were
a soft estimate.

## State and load

One mutex guards two maps, nodes and calls. Allocation is a read-modify-write — resolve affinity,
choose a node, record it — so it is one critical section, not three; an RWMutex would invite a
check-then-act race no detector can see.

The exercise does not define how `currentCalls` relates to placements this service has just made,
so the rule is plain: a report replaces the node's figure, an allocation increments it, a
termination decrements it. The local adjustment is what matters: trusting the last report alone
would send every call in a reporting interval to the same node.
**Limitation:** a late report overwrites adjustments made since it was generated; production would
need an explicit ownership protocol.

## Affinity and lifecycle

A call's node is recorded at placement and returned on every repeat, making allocation safely
retryable after a timeout. Affinity resolves before any capacity or region filter: flowing media
cannot be re-homed by editing a map. **Trade-off:** so a live `callId` re-allocated into a
*different* region still returns its pinned node, the mismatch logged rather than raised. A `409`
would serve the caller better but would break a stated requirement to satisfy an inferred one.
Termination frees the slot; an unknown call is `404`, though a retry-prone caller would prefer
`204`.

## One instance

State dies with the process, so the Deployment uses `Recreate`: at one replica a rolling update
still rounds `maxSurge` up to one pod, and two instances with disjoint call tables would briefly
share one Service. Liveness is deliberately slack — a restart destroys every mapping — while
readiness never gates on node count, since nodes register through the Service it controls.

**Node expiry is deliberately absent.** A node that stops reporting stays eligible, so calls can
go to one that is gone. Production would drop it after a few missed heartbeats and `lastSeen` is
reported for exactly that, but the brief defines no reporting interval and inventing one risks
rejecting a node the operator considers healthy.

**Also out of scope:** multiple replicas (affinity needs shared state), persistence, reaping
abandoned calls, cross-region overflow, node deregistration (`capacity: 0` drains), auth and TLS,
HPA and PDB, metrics.

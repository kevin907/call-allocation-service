# Design note

## Allocation

Within the requested region a call goes to the node with the greatest **absolute remaining
capacity** (`capacity - currentCalls`), ties broken on lowest node id so selection is
deterministic and testable. Full nodes are ineligible: capacity is a hard budget, not a queue.

The alternative is lowest utilisation (`currentCalls / capacity`), which spreads proportionally
across differently sized nodes. Neither dominates — headroom equalises free slots, utilisation
equalises percentages. I chose headroom because the exercise presents capacity as an absolute call
count; were it relative compute power, weighted utilisation would fit better.

## State and load

One mutex guards both maps. Allocation holds it across the affinity lookup, the selection and the
reservation, because those are one state transition — splitting them into separately locked read
and write stages is what would race. An RWMutex would be correct too, but buys little when the
main operation writes anyway.

The exercise does not define how `currentCalls` relates to placements just made, so the rule is
plain: a report replaces the node's figure, an allocation increments it, a termination decrements
it. The local adjustment is what matters — trusting the last report alone would send every call in
a reporting interval to the same node.
**Limitation:** a late report overwrites adjustments made since it was generated; production would
need an explicit ownership protocol.

## Affinity and lifecycle

A call's node is recorded at placement and returned on every repeat, making allocation safely
retryable after a timeout. Affinity resolves before any capacity or region filter: flowing media
cannot be re-homed by editing a map. **Trade-off:** a live `callId` re-allocated into a *different*
region therefore still returns its pinned node, the mismatch logged rather than raised. A `409`
would serve the caller better but breaks a stated requirement to satisfy an inferred one.
Termination frees the slot; an unknown call is `404`.

## One instance

State dies with the process, so the Deployment uses `Recreate`: at one replica a rolling update
still rounds `maxSurge` up to one pod, and two instances with disjoint call tables would briefly
share one Service. Liveness is deliberately slack, since a restart destroys every mapping.
Readiness reads no state and takes no lock: nodes register through the Service it controls, and
sharing the allocator's mutex would let load push the only pod out of the endpoints.

**Node expiry is deliberately absent.** A node that stops reporting stays eligible, so calls can
go to one that is gone. Production would drop it after a few missed heartbeats — `lastSeen` is
reported for that — but the brief defines no reporting interval, and inventing one risks rejecting
a node the operator considers healthy.

**Also out of scope:** multiple replicas (affinity needs shared state), persistence, reaping
abandoned calls, cross-region overflow, node deregistration (`capacity: 0` drains), auth and TLS,
HPA and PDB, metrics.

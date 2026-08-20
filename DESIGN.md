# Design note

## Choosing a node

Within the requested region the call goes to the healthy node with the most *absolute* free
capacity, ties broken on lowest node id. Full nodes are ineligible; media capacity is a hard
transcoding limit, not a queue, so refusing one call beats degrading a hundred.

Headroom rather than lowest utilisation ratio. Given nodes of 1000/900, 100/50 and 4/0, ratio
ordering sends the next four calls to the four-slot node — saturating it while 150 slots sit idle
— then thrashes, because one call moves a small node many percentage points and a large one
almost none. Headroom empties nodes together. If capacity were a soft estimate, lowest-utilisation
with a cutoff would be safer.

The tie-break makes selection deterministic — safe only because free capacity drops at
placement time.

## Counting load

A node reports `currentCalls` periodically, but we also know what we just placed. Trusting
the report alone is the failure that matters: every call between two heartbeats sees one snapshot
and picks one winner, stampeding one node while the fleet has room.

Each node therefore carries three numbers: the reported figure, `placed` (ours, still active) and
`external`. Every report rebases `external = max(0, reported − placed)` and never touches
`placed`; load is `placed + external`. The tempting `max(reported, placed)` agrees only at the
instant of a report — with reported 45 and placed 25, five further placements give 50 under the
rebase and 45 under the max, losing exactly the calls placed since.

This converges within one report interval rather than being exact: a report predating some of our
placements under-estimates `external` until the next one.

## Affinity, conflict, termination

A call's node is recorded at placement and returned on every repeat, making `POST /calls` safely
retryable after a timeout — what an at-least-once caller needs. Affinity resolves before any
health or capacity filter, because media already flowing cannot be re-homed by editing a map.

The brief's two rules collide when a live `callId` is re-allocated into a different region:
affinity demands the original node, region selection the requested one. Rather than break either
silently, the service returns `409` naming the existing pin.

Termination frees the slot and `404`s an unknown call: a retry-prone caller would prefer `204`,
but a mistyped `callId` is the commoner bug.

## Health and staleness

Nodes unheard from for `NODE_TTL` (30s, three report intervals) are excluded from new allocations
but retained, so one recovers on its next report. Staleness is judged at the decision point, not
by a reaper: no ticker, no window where a dead node is still selectable.

## One instance, in memory

State dies with the process, which is why the Deployment uses `Recreate`. At one replica a rolling
update still rounds `maxSurge` up to one pod, so two instances with disjoint call tables would
briefly share the Service and some allocations would come from an empty registry. Seconds of
downtime beat being silently wrong.

Liveness is deliberately slack — a restart destroys every mapping — while readiness never gates
on node count, since nodes register through the very Service it controls.

## Deliberately out of scope

- **Multiple replicas** — affinity would need shared state, which the brief excludes.
- **Persistence** — a restart is a cold start.
- **Reaping abandoned calls** — the map grows unbounded.
- **Cross-region overflow** — the brief says allocate within the region.
- **Node deregistration** — `capacity: 0` drains; the TTL covers death.
- **Auth, rate limiting, TLS** — internal service.
- **HPA and PodDisruptionBudget** — meaningless at one replica.
- **Metrics** — `GET /nodes` carries the numbers.

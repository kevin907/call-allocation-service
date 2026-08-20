# Call Allocation Service

Routes calls to conferencing nodes. Nodes report their capacity periodically; the service places
each call on a node in the requested region, keeps a call on the same node for its lifetime, and
frees the capacity when the call ends.

State is held in memory in a single instance, as the brief specifies. See [DESIGN.md](DESIGN.md)
for the decisions and their trade-offs.

The allocation policy is about forty lines in
[internal/allocation/allocator.go](internal/allocation/allocator.go), which is the place to look
first.

## Run it

Go 1.24 or newer. No third-party dependencies, so there is nothing to install.

```
git clone https://github.com/kevin907/call-allocation-service.git
cd call-allocation-service
go run ./cmd/callallocator
```

It listens on `:8080`.

## API

| Method | Path | Body | Success | Errors |
|---|---|---|---|---|
| `PUT` | `/nodes/{id}` | `{"id","region","capacity","currentCalls"}` | `201` first registration, `200` refresh | `400` `413` `415` |
| `GET` | `/nodes` | – | `200` fleet view | – |
| `POST` | `/calls` | `{"callId","region"}` | `201` placed (+ `Location`), `200` already active | `400` `413` `415` `503` |
| `GET` | `/calls/{callId}` | – | `200` | `400` `404` |
| `DELETE` | `/calls/{callId}` | – | `204` | `400` `404` |
| `GET` | `/healthz` | – | `200` liveness | – |
| `GET` | `/readyz` | – | `200` readiness, reports fleet size | – |

Errors are `{"error":"<code>","message":"<human>"}`. The codes are `invalid_request`,
`id_mismatch`, `call_not_found`, `no_nodes_in_region`, `no_capacity`, `payload_too_large`,
`unsupported_media_type` and `internal`.

`id` in the node body is optional; the path is authoritative and a disagreement is a `400`.
Unknown paths and wrong methods are answered by the standard library,
so those two responses are plain text rather than JSON — a deliberate trade against writing
middleware to reformat them.

## Walkthrough

Every command below is copy-pasteable, and the output is the service's own, with only the
timestamp elided.

Register three nodes across two regions:

```
curl -X PUT localhost:8080/nodes/node-eu-1 -H 'Content-Type: application/json' \
  -d '{"id":"node-eu-1","region":"eu-west","capacity":100,"currentCalls":20}'
curl -X PUT localhost:8080/nodes/node-eu-2 -H 'Content-Type: application/json' \
  -d '{"id":"node-eu-2","region":"eu-west","capacity":50,"currentCalls":0}'
curl -X PUT localhost:8080/nodes/node-us-1 -H 'Content-Type: application/json' \
  -d '{"id":"node-us-1","region":"us-east","capacity":10,"currentCalls":0}'
```

Allocate a call. `node-eu-1` wins with 80 free slots against `node-eu-2`'s 50, even though it is
the busier node in percentage terms — the policy ranks on absolute remaining capacity:

```
curl -X POST localhost:8080/calls -H 'Content-Type: application/json' \
  -d '{"callId":"abc123","region":"eu-west"}'

{"nodeId":"node-eu-1"}
```

Ask again and the same node comes back, without consuming a second slot:

```
curl -X POST localhost:8080/calls -H 'Content-Type: application/json' \
  -d '{"callId":"abc123","region":"eu-west"}'

{"nodeId":"node-eu-1"}
```

Affinity admits no exception. Even asked for a different region, an active call keeps its node —
the mismatch is logged for the operator rather than turned into an error for the caller:

```
curl -X POST localhost:8080/calls -H 'Content-Type: application/json' \
  -d '{"callId":"abc123","region":"us-east"}'

{"nodeId":"node-eu-1"}
```

A region nobody has registered in:

```
curl -X POST localhost:8080/calls -H 'Content-Type: application/json' \
  -d '{"callId":"zzz","region":"ap-south"}'

{"error":"no_nodes_in_region","message":"no node registered in region"}
```

The fleet view. `node-eu-1` last reported 20 calls and we have placed one on it since, so it is
holding 21 with 79 slots free:

```
curl localhost:8080/nodes

{"nodes":[{"id":"node-eu-1","region":"eu-west","capacity":100,"currentCalls":21,"available":79,"lastSeen":"..."}, ...]}
```

End the call, then end it again:

```
curl -i -X DELETE localhost:8080/calls/abc123    # 204 No Content
curl -i -X DELETE localhost:8080/calls/abc123    # 404 Not Found
```

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | listen port |
| `SHUTDOWN_TIMEOUT` | `10s` | how long in-flight requests get to finish after `SIGTERM` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

An unparseable value is a startup failure rather than a silent fallback.

A registered node stays eligible until it is replaced by a newer report. The brief does not define
a reporting interval, so the service does not invent one and never expires a node — see
[DESIGN.md](DESIGN.md).

## Docker

```
docker build -t call-allocation-service:dev .
docker run --rm -p 8080:8080 call-allocation-service:dev
```

The image is a static binary on `distroless/static`, about 8 MB, running as `nonroot`. It needs
no writable filesystem or capabilities:

```
docker run --rm -p 8080:8080 --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  call-allocation-service:dev
```

## Kubernetes

The manifests were applied to a local [kind](https://kind.sigs.k8s.io) cluster, the walkthrough
above was run against them through a port-forward, and the rollout behaviour described in
[DESIGN.md](DESIGN.md) was observed rather than assumed.

```
kind create cluster --name pexip
docker build -t call-allocation-service:dev .
kind load docker-image call-allocation-service:dev --name pexip

kubectl apply -f k8s/
kubectl -n call-allocation rollout status deploy/call-allocation-service
kubectl -n call-allocation port-forward svc/call-allocation-service 8080:80
```

The files are numbered because `kubectl apply -f k8s/` walks the directory in lexical order and
the namespace has to exist first. `imagePullPolicy: IfNotPresent` is set explicitly to say the
image comes from the node rather than a registry; a `:latest` tag would default to `Always` and
fail here. On minikube use `minikube image load` and on k3d `k3d image import` in place of
`kind load`.

Tear down with `kind delete cluster --name pexip`.

## Tests

```
go test -race -cover ./...      # unit and HTTP tests
./scripts/verify-k8s.sh         # end-to-end against a real cluster
```

The two Go tests worth reading are in
[internal/allocation/concurrent_test.go](internal/allocation/concurrent_test.go): one runs 100
goroutines allocating the *same* `callId` and asserts they all get one node and consume exactly
one slot, the other runs 200 concurrent allocations against 10 slots and asserts exactly 10
succeed. Both are the requirements that naive implementations get wrong.

[scripts/verify-k8s.sh](scripts/verify-k8s.sh) covers what unit tests cannot. It builds the
image, stands up a throwaway kind cluster, applies the manifests and makes 29 assertions about the
running deployment — that the Service selector actually matches a pod, that the probes pass, that
the container survives its own `securityContext`, that the API answers through the Service, that
a rollout never runs two pods at once, that state is gone afterwards, and that probe traffic
stays out of the log. It creates the cluster only if one is not already there, and removes it
again if it did.

A client-side dry-run proves only that the YAML parses; every failure listed above is invisible
to it. Both suites run in CI on pull requests and on pushes to `main` — see
[.github/workflows/ci.yml](.github/workflows/ci.yml).

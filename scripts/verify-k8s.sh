#!/usr/bin/env bash
#
# End-to-end verification of the Kubernetes deployment against a real cluster.
#
# A client-side dry-run only proves the YAML parses. The failures that actually
# bite are a Service selector matching no pod, a probe that never passes, a
# securityContext the binary cannot satisfy, and an image the node cannot see —
# none of which are visible without applying the manifests and watching what
# happens. This script asserts all of them.
#
# Creates a throwaway kind cluster if one is not already present, and removes it
# again only if it created it. Requires docker, kind and kubectl.
#
#   ./scripts/verify-k8s.sh

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-cas-verify}"
IMAGE="call-allocation-service:dev"
NS="call-allocation"
DEPLOY="deploy/call-allocation-service"
SVC="call-allocation-service"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

created_cluster=0
pf_pid=""
failures=0

cleanup() {
  [[ -n "${pf_pid:-}" ]] && kill "$pf_pid" 2>/dev/null || true
  if [[ "$created_cluster" == "1" ]]; then
    echo "--> removing kind cluster '$CLUSTER'"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

pass() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

check() {
  local desc="$1" want="$2" got="$3"
  if [[ "$got" == "$want" ]]; then pass "$desc"; else fail "$desc (want '$want', got '$got')"; fi
}

api() {
  curl -s -o /dev/null -w '%{http_code}' "$@"
}

port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# Ports get squatted on by unrelated local services, and a check that silently
# talks to the wrong server is worse than no check at all. Take a free port, then
# confirm the thing answering is actually this service before asserting anything.
start_port_forward() {
  for candidate in $(seq 18100 18160); do
    if port_free "$candidate"; then PORT="$candidate"; break; fi
  done
  if [[ -z "${PORT:-}" ]]; then fail "no free local port in 18100-18160"; return 1; fi

  kubectl -n "$NS" port-forward "svc/$SVC" "$PORT:80" >/tmp/cas-pf.log 2>&1 &
  pf_pid=$!

  for _ in $(seq 1 60); do
    if [[ "$(curl -s -m 1 "localhost:$PORT/healthz" 2>/dev/null)" == '{"status":"ok"}' ]]; then
      return 0
    fi
    sleep 0.5
  done

  fail "nothing recognisable as this service answered on port $PORT"
  cat /tmp/cas-pf.log
  return 1
}

stop_port_forward() {
  [[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null || true
  pf_pid=""
  PORT=""
}

step "Cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "reusing existing kind cluster '$CLUSTER'"
else
  echo "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" >/dev/null
  created_cluster=1
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

step "Manifests are valid against the live API server"
# Server-side dry-run runs admission and full schema validation, which the
# client-side form does not. The namespace has to exist first, because the API
# server cannot validate a namespaced object against a namespace that is absent.
kubectl apply -f k8s/00-namespace.yaml >/dev/null
if kubectl apply -f k8s/ --dry-run=server >/dev/null 2>/tmp/dryrun.err; then
  pass "kubectl apply --dry-run=server accepts every manifest"
else
  fail "server-side dry-run rejected the manifests: $(cat /tmp/dryrun.err)"
fi

step "Build and load the image"
docker build -q -t "$IMAGE" . >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1
pass "image built and loaded into the node"

step "Apply"
kubectl apply -f k8s/ >/dev/null
check "namespace exists" "$NS" "$(kubectl get ns "$NS" -o jsonpath='{.metadata.name}' 2>/dev/null)"
check "deployment exists" "1" "$(kubectl -n "$NS" get "$DEPLOY" -o jsonpath='{.spec.replicas}' 2>/dev/null)"
check "service is ClusterIP" "ClusterIP" "$(kubectl -n "$NS" get "svc/$SVC" -o jsonpath='{.spec.type}' 2>/dev/null)"

step "Rollout reaches Ready (proves the probes pass and the image runs)"
if kubectl -n "$NS" rollout status "$DEPLOY" --timeout=180s >/dev/null 2>&1; then
  pass "deployment became available"
else
  fail "deployment never became ready"
  kubectl -n "$NS" describe "$DEPLOY" | tail -30
  kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -20
fi

# An image the node cannot see is the classic local-cluster failure.
check "no image pull failure" "" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}' 2>/dev/null | grep -o 'ImagePullBackOff\|ErrImagePull' | head -1)"

# A liveness probe that is too aggressive shows up here and nowhere else.
check "container has not restarted" "0" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"

step "Service selector actually matches the pod"
# A selector typo produces a healthy pod, a healthy Service and zero traffic.
endpoints=""
for _ in $(seq 1 60); do
  endpoints="$(kubectl -n "$NS" get endpointslice \
    -l "kubernetes.io/service-name=$SVC" \
    -o jsonpath='{.items[*].endpoints[*].addresses[*]}' 2>/dev/null)"
  [[ -n "$endpoints" ]] && break
  sleep 1
done
if [[ -n "$endpoints" ]]; then
  pass "service has endpoints ($endpoints)"
else
  fail "service has no endpoints - the selector matches no pod"
fi

step "Security context is satisfiable, not just declared"
check "runs as non-root" "true" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[0].spec.securityContext.runAsNonRoot}' 2>/dev/null)"
check "root filesystem is read-only" "true" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[0].spec.containers[0].securityContext.readOnlyRootFilesystem}' 2>/dev/null)"
check "all capabilities dropped" "ALL" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[0].spec.containers[0].securityContext.capabilities.drop[0]}' 2>/dev/null)"
check "pod is Running despite the above" "Running" \
  "$(kubectl -n "$NS" get pods -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"

step "API works through the Service"
start_port_forward || exit 1
pass "reached this service through the Service on port $PORT"

B="localhost:$PORT"
J='Content-Type: application/json'

check "healthz" "200" "$(api "$B/healthz")"
check "readyz"  "200" "$(api "$B/readyz")"
check "register node-eu-1" "201" \
  "$(api -X PUT "$B/nodes/node-eu-1" -H "$J" -d '{"id":"node-eu-1","region":"eu-west","capacity":100,"currentCalls":20}')"
check "re-register is an update" "200" \
  "$(api -X PUT "$B/nodes/node-eu-1" -H "$J" -d '{"id":"node-eu-1","region":"eu-west","capacity":100,"currentCalls":20}')"
check "register node-eu-2" "201" \
  "$(api -X PUT "$B/nodes/node-eu-2" -H "$J" -d '{"region":"eu-west","capacity":50,"currentCalls":0}')"

first="$(curl -s -X POST "$B/calls" -H "$J" -d '{"callId":"abc123","region":"eu-west"}')"
check "allocation picks the node with most free capacity" '{"nodeId":"node-eu-1"}' "$first"
again="$(curl -s -X POST "$B/calls" -H "$J" -d '{"callId":"abc123","region":"eu-west"}')"
check "affinity returns the same node" "$first" "$again"
check "region conflict is refused" "409" \
  "$(api -X POST "$B/calls" -H "$J" -d '{"callId":"abc123","region":"us-east"}')"
check "unknown region" "503" \
  "$(api -X POST "$B/calls" -H "$J" -d '{"callId":"zzz","region":"ap-south"}')"
check "terminate" "204" "$(api -X DELETE "$B/calls/abc123")"
check "terminate again" "404" "$(api -X DELETE "$B/calls/abc123")"

stop_port_forward

step "Recreate strategy never runs two pods at once"
check "strategy is Recreate" "Recreate" \
  "$(kubectl -n "$NS" get "$DEPLOY" -o jsonpath='{.spec.strategy.type}' 2>/dev/null)"

kubectl -n "$NS" rollout restart "$DEPLOY" >/dev/null
max_pods=0
for _ in $(seq 1 60); do
  n="$(kubectl -n "$NS" get pods --no-headers 2>/dev/null | grep -c 'Running\|ContainerCreating' || true)"
  [[ "$n" -gt "$max_pods" ]] && max_pods="$n"
  kubectl -n "$NS" rollout status "$DEPLOY" --timeout=1s >/dev/null 2>&1 && break
  sleep 0.5
done
kubectl -n "$NS" rollout status "$DEPLOY" --timeout=120s >/dev/null

if [[ "$max_pods" -le 1 ]]; then
  pass "at most one pod existed at any point during the rollout (saw $max_pods)"
else
  fail "saw $max_pods concurrent pods - two instances with disjoint state shared the Service"
fi

step "State is lost on restart, as documented"
start_port_forward || exit 1
check "registry is empty after the restart" '{"nodes":[]}' "$(curl -s "localhost:$PORT/nodes")"
stop_port_forward

step "Probe traffic is not logged"
probe_lines="$(kubectl -n "$NS" logs "$DEPLOY" 2>/dev/null | grep -c 'healthz\|readyz' || true)"
check "no probe requests in the log" "0" "$probe_lines"

step "Result"
kubectl delete -f k8s/ --ignore-not-found >/dev/null 2>&1 || true
if [[ "$failures" -eq 0 ]]; then
  printf '\033[32mall checks passed\033[0m\n'
else
  printf '\033[31m%d check(s) failed\033[0m\n' "$failures"
fi
exit "$failures"

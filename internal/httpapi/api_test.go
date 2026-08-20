package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kevin907/call-allocation-service/internal/allocation"
	"github.com/kevin907/call-allocation-service/internal/httpapi"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	registry := allocation.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.NewServer(registry, log).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	code, _ := decode(t, resp)["error"].(string)
	return code
}

// The brief's example node object, used verbatim so a change to validation that
// would reject it fails here.
const briefNode = `{"id":"node-eu-1","region":"eu-west","capacity":100,"currentCalls":20}`

func registerNode(t *testing.T, srv *httptest.Server, id, region string, capacity, current int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": id, "region": region, "capacity": capacity, "currentCalls": current,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if resp := do(t, srv, http.MethodPut, "/nodes/"+id, string(body)); resp.StatusCode > 299 {
		t.Fatalf("register %s: status %d", id, resp.StatusCode)
	}
}

func TestUpsertNode_AcceptsTheBriefsExamplePayload(t *testing.T) {
	srv := newTestServer(t)

	resp := do(t, srv, http.MethodPut, "/nodes/node-eu-1", briefNode)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	got := decode(t, resp)
	if got["id"] != "node-eu-1" || got["region"] != "eu-west" {
		t.Errorf("got %v", got)
	}
	if got["capacity"] != float64(100) || got["available"] != float64(80) {
		t.Errorf("capacity/available = %v/%v, want 100/80", got["capacity"], got["available"])
	}

	// A refresh is an update, not a new registration.
	if resp := do(t, srv, http.MethodPut, "/nodes/node-eu-1", briefNode); resp.StatusCode != http.StatusOK {
		t.Errorf("refresh status = %d, want 200", resp.StatusCode)
	}
}

func TestUpsertNode_BodyIDIsOptionalButMustMatch(t *testing.T) {
	srv := newTestServer(t)

	omitted := `{"region":"eu-west","capacity":10,"currentCalls":0}`
	if resp := do(t, srv, http.MethodPut, "/nodes/node-a", omitted); resp.StatusCode != http.StatusCreated {
		t.Errorf("omitted body id: status = %d, want 201", resp.StatusCode)
	}

	mismatch := `{"id":"node-b","region":"eu-west","capacity":10,"currentCalls":0}`
	resp := do(t, srv, http.MethodPut, "/nodes/node-a", mismatch)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != "id_mismatch" {
		t.Errorf("error = %q, want id_mismatch", got)
	}
}

func TestUpsertNode_Validation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"missing region", `{"capacity":10,"currentCalls":0}`, "invalid_request"},
		{"missing capacity", `{"region":"eu-west","currentCalls":0}`, "invalid_request"},
		{"missing currentCalls", `{"region":"eu-west","capacity":10}`, "invalid_request"},
		{"negative capacity", `{"region":"eu-west","capacity":-1,"currentCalls":0}`, "invalid_request"},
		{"negative currentCalls", `{"region":"eu-west","capacity":10,"currentCalls":-1}`, "invalid_request"},
		{"blank region", `{"region":"   ","capacity":10,"currentCalls":0}`, "invalid_request"},
		{"unknown field", `{"region":"eu-west","capacity":10,"currentCalls":0,"capcity":5}`, "invalid_request"},
		{"malformed json", `{"region":`, "invalid_request"},
		{"trailing object", `{"region":"eu-west","capacity":1,"currentCalls":0}{}`, "invalid_request"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			resp := do(t, srv, http.MethodPut, "/nodes/node-a", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if got := errorCode(t, resp); got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

func TestUpsertNode_CurrentCallsAboveCapacityIsAccepted(t *testing.T) {
	srv := newTestServer(t)
	// The node is reporting the truth about a capacity cut, not making a mistake.
	resp := do(t, srv, http.MethodPut, "/nodes/node-a", `{"region":"eu-west","capacity":10,"currentCalls":25}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := decode(t, resp)["available"]; got != float64(-15) {
		t.Errorf("available = %v, want -15", got)
	}
}

func TestAllocate_ReturnsExactlyTheBriefsResponseShape(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-eu-1", "eu-west", 100, 20)

	resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"eu-west"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/calls/abc123" {
		t.Errorf("Location = %q, want /calls/abc123", got)
	}

	got := decode(t, resp)
	if len(got) != 1 {
		t.Errorf("response has %d fields %v, want exactly one", len(got), got)
	}
	if got["nodeId"] != "node-eu-1" {
		t.Errorf("nodeId = %v, want node-eu-1", got["nodeId"])
	}
}

func TestAllocate_AffinityIsIdempotent(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-a", "eu-west", 100, 0)
	registerNode(t, srv, "node-b", "eu-west", 100, 0)

	first := decode(t, do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"eu-west"}`))

	for range 3 {
		resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"eu-west"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("repeat status = %d, want 200", resp.StatusCode)
		}
		if got := decode(t, resp); got["nodeId"] != first["nodeId"] {
			t.Fatalf("nodeId = %v, want %v", got["nodeId"], first["nodeId"])
		}
	}
}

// Affinity admits no exception: an active call keeps its node even when the
// caller asks for a different region.
func TestAllocate_AffinityWinsOverTheRequestedRegion(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-eu", "eu-west", 100, 0)
	registerNode(t, srv, "node-us", "us-east", 100, 0)
	do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"eu-west"}`)

	resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"us-east"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := decode(t, resp)["nodeId"]; got != "node-eu" {
		t.Errorf("nodeId = %v, want node-eu", got)
	}

	// No capacity may be taken in the region that was asked for.
	for _, n := range decode(t, do(t, srv, http.MethodGet, "/nodes", ""))["nodes"].([]any) {
		node := n.(map[string]any)
		if node["id"] == "node-us" && node["placedCalls"] != float64(0) {
			t.Errorf("node-us placedCalls = %v, want 0", node["placedCalls"])
		}
	}
}

func TestAllocate_UnavailableRegions(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-full", "eu-west", 1, 1)

	tests := []struct {
		name     string
		region   string
		wantCode string
	}{
		{"region with no nodes", "ap-south", "no_nodes_in_region"},
		{"region at capacity", "eu-west", "no_capacity"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"c1","region":"`+tc.region+`"}`)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", resp.StatusCode)
			}
			if got := errorCode(t, resp); got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}
			if got := resp.Header.Get("Retry-After"); got != "5" {
				t.Errorf("Retry-After = %q, want 5", got)
			}
		})
	}
}

func TestAllocate_Validation(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-a", "eu-west", 10, 0)

	for _, body := range []string{
		`{"region":"eu-west"}`,
		`{"callId":"","region":"eu-west"}`,
		`{"callId":"   ","region":"eu-west"}`,
		`{"callId":"abc","region":""}`,
		`{"callId":"abc/../x","region":"eu-west"}`,
	} {
		resp := do(t, srv, http.MethodPost, "/calls", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestRegionMatchingIsCaseInsensitive(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-a", "EU-West", 10, 0)

	resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"abc","region":"eu-west"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201: region should match regardless of case", resp.StatusCode)
	}
}

func TestCallLifecycle(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-a", "eu-west", 1, 0)
	do(t, srv, http.MethodPost, "/calls", `{"callId":"abc123","region":"eu-west"}`)

	got := decode(t, do(t, srv, http.MethodGet, "/calls/abc123", ""))
	if got["callId"] != "abc123" || got["nodeId"] != "node-a" || got["region"] != "eu-west" {
		t.Errorf("got %v", got)
	}

	// The node is full until the call ends.
	if resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"other","region":"eu-west"}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	if resp := do(t, srv, http.MethodDelete, "/calls/abc123", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if resp := do(t, srv, http.MethodPost, "/calls", `{"callId":"other","region":"eu-west"}`); resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201: the slot should be free again", resp.StatusCode)
	}

	for _, path := range []string{"/calls/abc123", "/calls/never-existed"} {
		if resp := do(t, srv, http.MethodDelete, path, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s: status = %d, want 404", path, resp.StatusCode)
		}
		if resp := do(t, srv, http.MethodGet, path, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestListNodes_EmptyFleetIsAnEmptyArray(t *testing.T) {
	srv := newTestServer(t)

	body, err := io.ReadAll(do(t, srv, http.MethodGet, "/nodes", "").Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"nodes":[]`) {
		t.Errorf("got %s, want an empty array rather than null", bytes.TrimSpace(body))
	}
}

func TestContentTypeIsRequiredOnlyForRequestsWithABody(t *testing.T) {
	srv := newTestServer(t)
	registerNode(t, srv, "node-a", "eu-west", 10, 0)
	do(t, srv, http.MethodPost, "/calls", `{"callId":"abc","region":"eu-west"}`)

	// Bodyless methods must not be asked for a Content-Type.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/nodes"},
		{http.MethodGet, "/calls/abc"},
		{http.MethodDelete, "/calls/abc"},
	} {
		if resp := do(t, srv, tc.method, tc.path, ""); resp.StatusCode == http.StatusUnsupportedMediaType {
			t.Errorf("%s %s returned 415", tc.method, tc.path)
		}
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/calls", strings.NewReader(`{"callId":"x","region":"eu-west"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv := newTestServer(t)

	body := `{"callId":"` + strings.Repeat("a", 128<<10) + `","region":"eu-west"}`
	resp := do(t, srv, http.MethodPost, "/calls", body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != "payload_too_large" {
		t.Errorf("error = %q, want payload_too_large", got)
	}
}

func TestRoutingErrors(t *testing.T) {
	srv := newTestServer(t)

	if resp := do(t, srv, http.MethodGet, "/nope", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", resp.StatusCode)
	}

	resp := do(t, srv, http.MethodPost, "/nodes/node-a", `{"region":"eu-west","capacity":1,"currentCalls":0}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); !strings.Contains(got, http.MethodPut) {
		t.Errorf("Allow = %q, want it to mention PUT", got)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	srv := newTestServer(t)

	if resp := do(t, srv, http.MethodGet, "/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}

	// Readiness reports the fleet size but must never gate on it.
	resp := do(t, srv, http.MethodGet, "/readyz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz on an empty registry: status = %d, want 200", resp.StatusCode)
	}
	if got := decode(t, resp)["nodes"]; got != float64(0) {
		t.Errorf("nodes = %v, want 0", got)
	}

	registerNode(t, srv, "node-a", "eu-west", 10, 0)
	if got := decode(t, do(t, srv, http.MethodGet, "/readyz", ""))["nodes"]; got != float64(1) {
		t.Errorf("nodes = %v, want 1", got)
	}
}

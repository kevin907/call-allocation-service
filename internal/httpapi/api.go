// Package httpapi is the transport layer: it decodes requests, validates them,
// calls the allocation registry and maps domain errors onto HTTP statuses.
package httpapi

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kevin907/call-allocation-service/internal/allocation"
)

type Server struct {
	registry *allocation.Registry
	log      *slog.Logger
}

func NewServer(registry *allocation.Registry, log *slog.Logger) *Server {
	return &Server{registry: registry, log: log}
}

// Handler builds the routing table. Unmatched paths get stdlib's 404, and a
// wrong method gets its 405 with an Allow header, both for free.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /nodes/{id}", s.upsertNode)
	mux.HandleFunc("GET /nodes", s.listNodes)
	mux.HandleFunc("POST /calls", s.allocateCall)
	mux.HandleFunc("GET /calls/{callId}", s.getCall)
	mux.HandleFunc("DELETE /calls/{callId}", s.terminateCall)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	// Recovery sits inside the logger so a panicking request still produces one
	// request line, with the 500 the client actually received.
	return logRequests(s.log, recoverPanics(s.log, mux))
}

// nodeRequest is the capacity report a node sends. The counts are pointers so an
// absent field is distinguishable from a deliberate zero.
type nodeRequest struct {
	ID           string `json:"id"`
	Region       string `json:"region"`
	Capacity     *int   `json:"capacity"`
	CurrentCalls *int   `json:"currentCalls"`
}

type allocateRequest struct {
	CallID string `json:"callId"`
	Region string `json:"region"`
}

// allocateResponse carries exactly the one field the brief specifies. Everything
// richer about a call is a GET away.
type allocateResponse struct {
	NodeID string `json:"nodeId"`
}

type callResponse struct {
	CallID      string    `json:"callId"`
	NodeID      string    `json:"nodeId"`
	Region      string    `json:"region"`
	AllocatedAt time.Time `json:"allocatedAt"`
}

func (s *Server) upsertNode(w http.ResponseWriter, r *http.Request) {
	var req nodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	id, err := validateID("node id", r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// The brief's example payload carries the id, so accept it, but the path is
	// authoritative and a disagreement means the caller has a bug worth hearing about.
	if bodyID := strings.TrimSpace(req.ID); bodyID != "" && bodyID != id {
		s.writeError(w, r, statusError{http.StatusBadRequest, codeIDMismatch,
			"body id does not match the path"})
		return
	}

	region, err := validateRegion(req.Region)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	capacity, err := validateCount("capacity", req.Capacity)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	currentCalls, err := validateCount("currentCalls", req.CurrentCalls)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	created := s.registry.UpsertNode(allocation.Report{
		ID:           id,
		Region:       region,
		Capacity:     capacity,
		CurrentCalls: currentCalls,
	})

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	node, _ := s.registry.Node(id)
	writeJSON(w, status, node)
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.registry.Snapshot()})
}

func (s *Server) allocateCall(w http.ResponseWriter, r *http.Request) {
	var req allocateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	callID, err := validateID("callId", req.CallID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	region, err := validateRegion(req.Region)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	a, created, err := s.registry.Allocate(callID, region)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// The call keeps its node whatever region the caller now asks for. That is
	// what the affinity requirement demands, but a caller naming a different
	// region than the one the call is pinned in usually has a bug, so say so.
	if a.Region != region {
		s.log.WarnContext(r.Context(), "call is pinned in a different region than requested",
			"callId", a.CallID, "nodeId", a.NodeID, "pinnedRegion", a.Region, "requestedRegion", region)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/calls/"+url.PathEscape(a.CallID))
	}
	writeJSON(w, status, allocateResponse{NodeID: a.NodeID})
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request) {
	callID, err := validateID("callId", r.PathValue("callId"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	a, err := s.registry.Get(callID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, callResponse{
		CallID:      a.CallID,
		NodeID:      a.NodeID,
		Region:      a.Region,
		AllocatedAt: a.AllocatedAt,
	})
}

func (s *Server) terminateCall(w http.ResponseWriter, r *http.Request) {
	callID, err := validateID("callId", r.PathValue("callId"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	if err := s.registry.Terminate(callID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// healthz answers whether the process is alive. It deliberately asserts nothing
// else: a restart here destroys every call-to-node mapping, so this must not be
// the thing that triggers one.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports the fleet size but never gates on it. Nodes register through
// the same Service readiness controls, so refusing traffic while the registry is
// empty would be a deadlock the service could not escape.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"nodes":  s.registry.NodeCount(),
	})
}

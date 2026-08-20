package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/kevin907/call-allocation-service/internal/allocation"
)

// Bodies here are four small fields; anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

// The complete set of machine-readable error codes.
const (
	codeInvalidRequest   = "invalid_request"
	codeIDMismatch       = "id_mismatch"
	codeRegionMismatch   = "region_mismatch"
	codeCallNotFound     = "call_not_found"
	codeNoNodesInRegion  = "no_nodes_in_region"
	codeNoCapacity       = "no_capacity"
	codePayloadTooLarge  = "payload_too_large"
	codeUnsupportedMedia = "unsupported_media_type"
	codeInternal         = "internal"
)

// apiError is the one error shape the service returns.
type apiError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

// statusError lets a handler return a single value and leave the status, code
// and wording to writeError.
type statusError struct {
	status  int
	code    string
	message string
}

func (e statusError) Error() string { return e.message }

func badRequest(format string, args ...any) statusError {
	return statusError{http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf(format, args...)}
}

// decodeJSON reads exactly one JSON object from the request body. Unknown fields
// are rejected because the alternative is worse: a node that misspells
// "capacity" would register with none and silently stop receiving calls.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return statusError{http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)}
		}
		return badRequest("malformed JSON: %v", err)
	}
	if dec.More() {
		return badRequest("body must contain a single JSON object")
	}
	return nil
}

func requireJSONContentType(r *http.Request) error {
	unsupported := statusError{http.StatusUnsupportedMediaType, codeUnsupportedMedia,
		"Content-Type must be application/json"}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return unsupported
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return unsupported
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// writeError is the only place domain errors become HTTP statuses; nothing below
// this package knows what a status code is.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		status  int
		code    string
		message = err.Error()
	)

	var explicit statusError
	var conflict *allocation.RegionConflictError

	switch {
	case errors.As(err, &explicit):
		status, code, message = explicit.status, explicit.code, explicit.message
	case errors.As(err, &conflict):
		status, code = http.StatusConflict, codeRegionMismatch
	case errors.Is(err, allocation.ErrCallNotFound):
		status, code = http.StatusNotFound, codeCallNotFound
	case errors.Is(err, allocation.ErrNoNodesInRegion):
		status, code = http.StatusServiceUnavailable, codeNoNodesInRegion
	case errors.Is(err, allocation.ErrNoCapacity):
		status, code = http.StatusServiceUnavailable, codeNoCapacity
	default:
		s.log.ErrorContext(r.Context(), "unhandled error", "method", r.Method, "path", r.URL.Path, "err", err)
		status, code, message = http.StatusInternalServerError, codeInternal, "internal error"
	}

	// Both 503s are transient, so say so rather than leaving the client to guess.
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, apiError{Code: code, Message: message})
}

const maxIDLen = 128

// identifiers are restricted to characters that survive a URL path untouched, so
// what an operator sees in a log is what the caller sent.
func validateID(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "", badRequest("%s is required", field)
	case len(value) > maxIDLen:
		return "", badRequest("%s must be at most %d characters", field, maxIDLen)
	}
	for _, c := range value {
		isAllowed := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !isAllowed {
			return "", badRequest("%s may only contain letters, digits, '-', '_' and '.'", field)
		}
	}
	return value, nil
}

// Region is a matching key, so a node registering "EU-West" must land in the
// same bucket as a call asking for "eu-west". Normalising once here beats a
// silent "no nodes in region" that nobody can explain.
func validateRegion(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "":
		return "", badRequest("region is required")
	case len(value) > 64:
		return "", badRequest("region must be at most 64 characters")
	}
	return value, nil
}

// A pointer distinguishes an absent field from a legitimate zero: capacity 0
// means a drained node, not a missing value.
func validateCount(field string, value *int) (int, error) {
	switch {
	case value == nil:
		return 0, badRequest("%s is required", field)
	case *value < 0:
		return 0, badRequest("%s must not be negative", field)
	}
	return *value, nil
}

// Package api serves the panel and the automation API: REST JSON under
// /api/v1 described with OpenAPI, a multiplexed WebSocket for live state,
// and the embedded React panel. The panel (session cookie) and automation
// (API tokens, sent as Bearer) share the same endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/app"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/pkg"
)

// Problem is an RFC 9457 problem document, with MockVision extensions.
type Problem struct {
	Type          string              `json:"type"`
	Title         string              `json:"title"`
	Status        int                 `json:"status"`
	Detail        string              `json:"detail,omitempty"`
	Instance      string              `json:"instance,omitempty"`
	Code          string              `json:"code,omitempty"`
	Errors        []domain.FieldError `json:"errors,omitempty"`
	Report        *pkg.Report         `json:"report,omitempty"`
	SetupRequired *bool               `json:"setup_required,omitempty"`
}

const problemType = "urn:mockvision:problem:"

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Instance == "" {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// writeError maps domain and app errors to problems.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	p := s.problemFor(r, err)
	if errors.Is(err, app.ErrInvalidToken) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	writeProblem(w, r, p)
}

// problemFor maps an error to its problem document.
func (s *Server) problemFor(r *http.Request, err error) Problem {
	var (
		verr   *domain.ValidationError
		cerr   *domain.ConflictError
		rerr   *domain.RejectedError
		ierr   *app.ImportError
		locked *app.LockedError
		berr   *badRequest
	)
	switch {
	case errors.As(err, &berr):
		return Problem{Type: problemType + "bad-request", Title: "Bad request", Status: http.StatusBadRequest, Detail: berr.msg}
	case errors.As(err, &verr):
		return Problem{Type: problemType + "validation", Title: "Invalid input", Status: http.StatusUnprocessableEntity, Detail: verr.Error(), Errors: verr.Fields}
	case errors.As(err, &ierr):
		rep := ierr.Report
		return Problem{Type: problemType + "package-rejected", Title: "Package rejected", Status: http.StatusUnprocessableEntity, Detail: ierr.Error(), Report: &rep}
	case errors.As(err, &rerr):
		return Problem{Type: problemType + "admission-rejected", Title: "Not enough capacity", Status: http.StatusConflict, Detail: rerr.Reason, Code: rerr.Code}
	case errors.As(err, &cerr):
		p := Problem{Type: problemType + "conflict", Title: "Conflict", Status: http.StatusConflict, Detail: cerr.Message}
		if cerr.Field != "" {
			p.Errors = []domain.FieldError{{Field: cerr.Field, Message: cerr.Message}}
		}
		return p
	case errors.Is(err, domain.ErrNotFound):
		return Problem{Type: problemType + "not-found", Title: "Not found", Status: http.StatusNotFound}
	case errors.Is(err, app.ErrInvalidToken):
		return Problem{Type: problemType + "invalid-token", Title: "Invalid API token", Status: http.StatusUnauthorized, Detail: err.Error()}
	case errors.Is(err, app.ErrUnauthenticated):
		return Problem{Type: problemType + "unauthenticated", Title: "Authentication required", Status: http.StatusUnauthorized, Detail: "wrong username or password"}
	case errors.As(err, &locked):
		return Problem{Type: problemType + "locked", Title: "Too many attempts", Status: http.StatusTooManyRequests, Detail: locked.Error()}
	case errors.Is(err, app.ErrSetupDone):
		return Problem{Type: problemType + "setup-done", Title: "Setup already completed", Status: http.StatusConflict}
	case errors.Is(err, context.DeadlineExceeded):
		return Problem{Type: problemType + "timeout", Title: "Timed out", Status: http.StatusGatewayTimeout}
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		return Problem{Type: problemType + "internal", Title: "Internal error", Status: http.StatusInternalServerError, Detail: "see the node logs"}
	}
}

type badRequest struct{ msg string }

func (b *badRequest) Error() string { return b.msg }

func badReq(msg string) error { return &badRequest{msg: msg} }

// maxJSONBody bounds JSON request bodies.
const maxJSONBody = 1 << 20

// decode reads a JSON body strictly: unknown fields are errors.
func decode(r *http.Request, v any) error {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		return badReq("the body must be application/json")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field") {
			msg = strings.TrimPrefix(msg, "json: ")
		}
		return badReq("invalid JSON: " + msg)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("response write failed", "error", err)
	}
}

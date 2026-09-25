package api

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/app"
)

//go:embed openapi.yaml
var openAPISpec []byte

// Config configures the HTTP layer.
type Config struct {
	// AllowedOrigins are extra origins (host[:port]) allowed for the
	// WebSocket and mutating requests, such as a Vite dev server.
	AllowedOrigins []string
	// SecureCookies marks session cookies Secure (behind HTTPS).
	SecureCookies bool
}

// Server wires the HTTP API to the service.
type Server struct {
	cfg    Config
	svc    *app.Service
	hub    *Hub
	log    *slog.Logger
	static fs.FS
}

// New builds a server; static is the built panel (frontend/dist).
func New(cfg Config, svc *app.Service, hub *Hub, static fs.FS, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, svc: svc, hub: hub, log: log, static: static}
}

const sessionCookie = "mv_session"

type ctxKey int

const sessionKey ctxKey = 1

func sessionFrom(ctx context.Context) (app.Session, bool) {
	s, ok := ctx.Value(sessionKey).(app.Session)
	return s, ok
}

// actor is the authenticated user of a request, for the audit log: the
// panel with a session, the API with a token.
func actor(r *http.Request) app.Actor {
	a := app.Actor{Type: "user", IP: clientIP(r), Origin: app.OriginPanel}
	if s, ok := sessionFrom(r.Context()); ok {
		a.ID, a.Name = s.User.ID, s.User.Username
		if s.Token != nil {
			a.Origin, a.Token = app.OriginAPI, s.Token
		}
	}
	return a
}

// bearerToken returns the token of an Authorization: Bearer header. ok
// reports whether the request has an Authorization header at all: such a
// request is authenticated by that header alone, never by the cookie.
func bearerToken(r *http.Request) (token string, ok bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(token), true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	pub := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, h) }
	auth := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireAuth(h)) }
	// Tokens are managed from the panel only: a leaked token cannot mint
	// or revoke others.
	panel := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireAuth(s.panelOnly(h))) }

	pub("POST /api/v1/auth/setup", s.handleSetup)
	pub("POST /api/v1/auth/login", s.handleLogin)
	pub("POST /api/v1/auth/logout", s.handleLogout)
	pub("GET /api/v1/auth/me", s.handleMe)
	pub("GET /api/v1/openapi.yaml", s.handleOpenAPI)

	auth("GET /api/v1/node", s.handleNode)
	auth("GET /api/v1/node/metrics", s.handleNodeMetrics)
	auth("GET /api/v1/node/history", s.handleNodeHistory)
	auth("GET /api/v1/settings", s.handleGetSettings)
	auth("PATCH /api/v1/settings", s.handlePatchSettings)

	auth("POST /api/v1/packages", s.handleImportPackage)
	auth("GET /api/v1/profiles", s.handleListProfiles)
	auth("GET /api/v1/profiles/{vendor}/{model}/versions/{version}", s.handleGetProfile)
	auth("POST /api/v1/profiles/{vendor}/{model}/versions/{version}/actions/{action}", s.handleProfileAction)

	panel("GET /api/v1/tokens", s.handleListTokens)
	panel("POST /api/v1/tokens", s.handleCreateToken)
	panel("DELETE /api/v1/tokens/{id}", s.handleRevokeToken)

	auth("GET /api/v1/audit", s.handleListAudit)

	auth("GET /api/v1/cameras", s.handleListCameras)
	auth("POST /api/v1/cameras", s.handleCreateCamera)
	auth("POST /api/v1/cameras/actions/bulk", s.handleBulkCameras)
	auth("GET /api/v1/cameras/{id}", s.handleGetCamera)
	auth("PATCH /api/v1/cameras/{id}", s.handleUpdateCamera)
	auth("DELETE /api/v1/cameras/{id}", s.handleDeleteCamera)
	auth("POST /api/v1/cameras/{id}/actions/{action}", s.handleCameraAction)
	auth("POST /api/v1/cameras/{id}/actions/factory-reset", s.handleResetCamera)
	auth("POST /api/v1/cameras/{id}/actions/clone", s.handleCloneCamera)
	auth("PUT /api/v1/cameras/{id}/users", s.handleSetCameraUsers)
	auth("PUT /api/v1/cameras/{id}/protocols", s.handleSetCameraProtocols)
	auth("PATCH /api/v1/cameras/{id}/streams/{stream}", s.handleUpdateCameraStream)
	auth("GET /api/v1/cameras/{id}/status", s.handleCameraStatus)
	auth("GET /api/v1/cameras/{id}/config", s.handleGetConfig)
	auth("PATCH /api/v1/cameras/{id}/config", s.handlePatchConfig)
	auth("GET /api/v1/cameras/{id}/metrics", s.handleCameraMetrics)
	auth("GET /api/v1/cameras/{id}/snapshot", s.handleSnapshot)
	auth("POST /api/v1/cameras/{id}/events", s.handleTrigger)

	auth("GET /api/v1/assets", s.handleListAssets)
	auth("POST /api/v1/assets", s.handleUploadAsset)
	auth("GET /api/v1/assets/{id}/content", s.handleAssetContent)
	auth("DELETE /api/v1/assets/{id}", s.handleDeleteAsset)

	auth("GET /api/v1/targets", s.handleListTargets)
	auth("POST /api/v1/targets", s.handleCreateTarget)
	auth("GET /api/v1/targets/{id}", s.handleGetTarget)
	auth("PATCH /api/v1/targets/{id}", s.handleUpdateTarget)
	auth("DELETE /api/v1/targets/{id}", s.handleDeleteTarget)
	auth("POST /api/v1/targets/{id}/actions/test", s.handleTestTarget)

	auth("GET /api/v1/events", s.handleListEvents)
	auth("GET /api/v1/events/{id}", s.handleGetEvent)

	auth("GET /api/v1/ws", s.serveWS)

	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, Problem{Type: problemType + "not-found", Title: "Not found", Status: http.StatusNotFound})
	})

	root := http.NewServeMux()
	root.Handle("/api/", s.csrf(api))
	root.Handle("/", spaHandler(s.static))
	return s.recoverer(securityHeaders(root))
}

// requireAuth accepts an API token (Authorization: Bearer) or a session
// cookie. A request with a token is judged by the token alone, and a read
// token may only make safe requests (D54).
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.authenticate(r)
		if err != nil {
			if errors.Is(err, app.ErrInvalidToken) {
				s.writeError(w, r, err)
				return
			}
			s.unauthenticated(w, r)
			return
		}
		if sess.Token != nil && !safeMethod(r.Method) && !sess.Token.CanWrite() {
			writeProblem(w, r, Problem{Type: problemType + "insufficient-scope", Title: "Insufficient scope", Status: http.StatusForbidden,
				Detail: "this API token can only read; create one with the write scope to change the node"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

// authenticate resolves the token or the session cookie of a request.
func (s *Server) authenticate(r *http.Request) (app.Session, error) {
	if token, ok := bearerToken(r); ok {
		return s.svc.AuthenticateToken(r.Context(), token, clientIP(r))
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return app.Session{}, app.ErrUnauthenticated
	}
	return s.svc.Authenticate(r.Context(), c.Value)
}

// panelOnly refuses requests authenticated with an API token.
func (s *Server) panelOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := sessionFrom(r.Context()); ok && sess.Token != nil {
			writeProblem(w, r, Problem{Type: problemType + "forbidden", Title: "Not allowed with an API token", Status: http.StatusForbidden,
				Detail: "API tokens are managed from the panel"})
			return
		}
		next(w, r)
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request) {
	setup, _ := s.svc.SetupRequired(r.Context())
	writeProblem(w, r, Problem{Type: problemType + "unauthenticated", Title: "Authentication required", Status: http.StatusUnauthorized, SetupRequired: &setup})
}

// csrf protects cookie-authenticated requests: SameSite=Strict cookies, an
// Origin check, and a custom header that cross-site forms cannot send.
// Requests with an Authorization header are exempt: a browser never adds
// one on its own, and those requests ignore the cookie.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, bearer := bearerToken(r); bearer || safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin, r.Host) {
			writeProblem(w, r, Problem{Type: problemType + "forbidden", Title: "Cross-origin request refused", Status: http.StatusForbidden})
			return
		}
		if r.Header.Get("X-MockVision-Request") == "" {
			writeProblem(w, r, Problem{Type: problemType + "forbidden", Title: "Missing X-MockVision-Request header", Status: http.StatusForbidden,
				Detail: "state-changing requests must send the X-MockVision-Request header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	for _, o := range s.cfg.AllowedOrigins {
		if strings.EqualFold(o, u.Host) {
			return true
		}
	}
	return false
}

// contentSecurityPolicy keeps the panel strict: no inline scripts or
// styles, nothing from other origins (the app never uses a CDN).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Browsers ignore COOP on plain HTTP and log an error for it; the
		// panel is often opened by IP on a lab network.
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
		}
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				writeProblem(w, r, Problem{Type: problemType + "internal", Title: "Internal error", Status: http.StatusInternalServerError})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: exp, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies})
}

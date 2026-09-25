package api

import (
	"context"
	_ "embed"
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

// actor is the authenticated user of a request, for the audit log.
func actor(r *http.Request) app.Actor {
	a := app.Actor{Type: "user", IP: clientIP(r)}
	if s, ok := sessionFrom(r.Context()); ok {
		a.ID, a.Name = s.User.ID, s.User.Username
	}
	return a
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
	auth := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireSession(h)) }

	pub("POST /api/v1/auth/setup", s.handleSetup)
	pub("POST /api/v1/auth/login", s.handleLogin)
	pub("POST /api/v1/auth/logout", s.handleLogout)
	pub("GET /api/v1/auth/me", s.handleMe)
	pub("GET /api/v1/openapi.yaml", s.handleOpenAPI)

	auth("GET /api/v1/node", s.handleNode)
	auth("GET /api/v1/node/metrics", s.handleNodeMetrics)
	auth("GET /api/v1/settings", s.handleGetSettings)
	auth("PATCH /api/v1/settings", s.handlePatchSettings)

	auth("POST /api/v1/packages", s.handleImportPackage)
	auth("GET /api/v1/profiles", s.handleListProfiles)
	auth("GET /api/v1/profiles/{vendor}/{model}/versions/{version}", s.handleGetProfile)
	auth("POST /api/v1/profiles/{vendor}/{model}/versions/{version}/actions/{action}", s.handleProfileAction)

	auth("GET /api/v1/cameras", s.handleListCameras)
	auth("POST /api/v1/cameras", s.handleCreateCamera)
	auth("GET /api/v1/cameras/{id}", s.handleGetCamera)
	auth("PATCH /api/v1/cameras/{id}", s.handleUpdateCamera)
	auth("DELETE /api/v1/cameras/{id}", s.handleDeleteCamera)
	auth("POST /api/v1/cameras/{id}/actions/{action}", s.handleCameraAction)
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

// requireSession rejects requests without a valid session cookie.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			s.unauthenticated(w, r)
			return
		}
		sess, err := s.svc.Authenticate(r.Context(), c.Value)
		if err != nil {
			s.unauthenticated(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request) {
	setup, _ := s.svc.SetupRequired(r.Context())
	writeProblem(w, r, Problem{Type: problemType + "unauthenticated", Title: "Authentication required", Status: http.StatusUnauthorized, SetupRequired: &setup})
}

// csrf protects cookie-authenticated requests: SameSite=Strict cookies, an
// Origin check, and a custom header that cross-site forms cannot send.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
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
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
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

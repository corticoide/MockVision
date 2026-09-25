package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/crypto/argon2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Actor is who performs an operation, for the audit log (RN-08).
type Actor struct {
	Type string // user, camera or system
	ID   string
	Name string
	IP   string
	// Origin is where the change came from: panel, api, camera or system.
	// Empty derives it from Type, with users on the panel.
	Origin string
	// Token is the API token of a request made with one.
	Token *TokenInfo
}

// Origins of a change, as the audit log records them.
const (
	OriginPanel  = "panel"
	OriginAPI    = "api"
	OriginCamera = "camera"
	OriginSystem = "system"
)

func (a Actor) origin() string {
	switch {
	case a.Origin != "":
		return a.Origin
	case a.Type == "camera":
		return OriginCamera
	case a.Type == "system":
		return OriginSystem
	}
	return OriginPanel
}

// SystemActor is used for automatic operations such as the reconciler.
var SystemActor = Actor{Type: "system", ID: "reconciler"}

// Session is an authenticated request: a panel session, or an API token
// when Token is set.
type Session struct {
	User      domain.User
	ExpiresAt time.Time
	Token     *TokenInfo
}

// SessionTTL is the idle lifetime of a panel session.
const SessionTTL = 12 * time.Hour

// ErrUnauthenticated is returned for missing or invalid credentials.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrSetupDone is returned when the first administrator already exists.
var ErrSetupDone = errors.New("setup already completed")

// LockedError is returned while a login is locked after failed attempts.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many failed attempts; try again in %s", time.Until(e.Until).Round(time.Second))
}

// SetupRequired reports whether the first-run wizard must create the
// administrator: there are no default credentials.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.store.R().CountUsers(ctx)
	return n == 0, err
}

// Setup creates the first administrator and logs it in.
func (s *Service) Setup(ctx context.Context, username, password, ip, ua string) (token string, sess Session, err error) {
	if err := domain.ValidateUsername("username", username); err != nil {
		return "", Session{}, err
	}
	if err := domain.ValidatePanelPassword(password); err != nil {
		return "", Session{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", Session{}, err
	}
	u := domain.User{ID: ulid.Make().String(), Username: username, Role: domain.RoleAdmin, CreatedAt: time.Now()}
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		n, err := q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		return q.CreateUser(ctx, db.CreateUserParams{ID: u.ID, Username: u.Username, PasswordHash: hash, Role: string(u.Role), CreatedAt: u.CreatedAt.UnixMilli()})
	})
	if err != nil {
		return "", Session{}, err
	}
	s.audit(ctx, Actor{Type: "user", ID: u.ID, Name: u.Username, IP: ip}, "auth.setup", "user", u.ID, nil)
	return s.newSession(ctx, u, ip, ua)
}

// Login checks credentials and opens a session.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (string, Session, error) {
	key := strings.ToLower(username) + "|" + ip
	if until, locked := s.login.locked(key); locked {
		return "", Session{}, &LockedError{Until: until}
	}
	row, err := s.store.R().GetUserByUsername(ctx, username)
	if err != nil {
		// Spend the same time as a real check.
		_ = verifyPassword(dummyHash(), password)
		s.login.fail(key)
		return "", Session{}, ErrUnauthenticated
	}
	if !verifyPassword(row.PasswordHash, password) || store.Bool(row.Disabled) {
		s.login.fail(key)
		s.audit(ctx, Actor{Type: "user", Name: username, IP: ip}, "auth.login_failed", "user", row.ID, nil)
		return "", Session{}, ErrUnauthenticated
	}
	s.login.success(key)
	u := domain.User{ID: row.ID, Username: row.Username, Role: domain.Role(row.Role), CreatedAt: store.Time(row.CreatedAt)}
	s.audit(ctx, Actor{Type: "user", ID: u.ID, Name: u.Username, IP: ip}, "auth.login", "user", u.ID, nil)
	return s.newSession(ctx, u, ip, ua)
}

func (s *Service) newSession(ctx context.Context, u domain.User, ip, ua string) (string, Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	exp := time.Now().Add(SessionTTL)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	err := s.store.W().CreateSession(ctx, db.CreateSessionParams{
		ID: tokenID(token), UserID: u.ID, ExpiresAt: exp.UnixMilli(), Ip: ip, UserAgent: ua, CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return "", Session{}, err
	}
	return token, Session{User: u, ExpiresAt: exp}, nil
}

// Authenticate resolves a session token, extending it while in use.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" || len(token) > 100 {
		return Session{}, ErrUnauthenticated
	}
	id := tokenID(token)
	row, err := s.store.R().GetSession(ctx, id)
	if err != nil {
		return Session{}, ErrUnauthenticated
	}
	exp := store.Time(row.ExpiresAt)
	if time.Now().After(exp) || store.Bool(row.Disabled) {
		return Session{}, ErrUnauthenticated
	}
	// Extend at most every hour to spare writes on SD cards.
	if time.Until(exp) < SessionTTL-time.Hour {
		exp = time.Now().Add(SessionTTL)
		_ = s.store.W().ExtendSession(ctx, db.ExtendSessionParams{ID: id, ExpiresAt: exp.UnixMilli()})
	}
	return Session{User: domain.User{ID: row.UserID, Username: row.Username, Role: domain.Role(row.Role)}, ExpiresAt: exp}, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.W().DeleteSession(ctx, tokenID(token))
}

func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Argon2id parameters: about 100 ms and 64 MiB per login on a Raspberry Pi 4.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// dummyHash is checked when a user does not exist, so failed logins take
// the same time either way. It is computed on first use: Argon2id needs
// 64 MiB, which camera processes must not pay at start-up.
var dummyHash = sync.OnceValue(func() string {
	h, _ := hashPassword("mockvision-dummy-password")
	return h
})

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// loginGuard locks a username and IP pair after repeated failures, with a
// growing lock time.
type loginGuard struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
}

type loginEntry struct {
	failures int
	until    time.Time
}

func newLoginGuard() *loginGuard { return &loginGuard{entries: map[string]*loginEntry{}} }

func (g *loginGuard) locked(key string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[key]
	if e == nil || time.Now().After(e.until) {
		return time.Time{}, false
	}
	return e.until, true
}

func (g *loginGuard) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entries) > 10_000 {
		g.entries = map[string]*loginEntry{}
	}
	e := g.entries[key]
	if e == nil {
		e = &loginEntry{}
		g.entries[key] = e
	}
	e.failures++
	if e.failures >= 5 {
		lock := 30 * time.Second << (e.failures - 5)
		if lock > 15*time.Minute {
			lock = 15 * time.Minute
		}
		e.until = time.Now().Add(lock)
	}
}

func (g *loginGuard) success(key string) {
	g.mu.Lock()
	delete(g.entries, key)
	g.mu.Unlock()
}

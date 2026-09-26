package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// ErrSetupCode is returned when the setup code is missing or wrong.
var ErrSetupCode = errors.New("the setup code is missing or wrong; it is printed in the node's log and saved in the setup-code file of the data directory")

// setupCodeFile holds the one-time code that creating the administrator
// needs, so whoever reaches the panel first cannot claim the node (audit
// M1). It is removed once the administrator exists.
const setupCodeFile = "setup-code"

// SetupCode returns the one-time code of the first-run wizard, creating it
// when needed, and the file that keeps it. It is empty once the setup is
// done.
func (s *Service) SetupCode(ctx context.Context) (code, path string, err error) {
	path = filepath.Join(s.opts.DataDir, setupCodeFile)
	required, err := s.SetupRequired(ctx)
	if err != nil || !required {
		return "", path, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupCode != "" {
		return s.setupCode, path, nil
	}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		s.setupCode = strings.TrimSpace(string(b))
		return s.setupCode, path, nil
	}
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return "", path, err
	}
	// Base32 without padding: easy to read from a log and to type.
	code = strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
		return "", path, err
	}
	s.setupCode = code
	return code, path, nil
}

// announceSetup logs the setup code while the node has no administrator.
func (s *Service) announceSetup(ctx context.Context) {
	code, path, err := s.SetupCode(ctx)
	switch {
	case err != nil:
		s.log.Error("cannot create the setup code", "error", err)
	case code != "":
		s.log.Warn("first run: create the administrator in the panel with this setup code", "setup_code", code, "file", path)
	}
}

func (s *Service) checkSetupCode(ctx context.Context, given string) error {
	code, _, err := s.SetupCode(ctx)
	if err != nil {
		return err
	}
	given = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(given), " ", ""))
	if code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(given)) != 1 {
		return ErrSetupCode
	}
	return nil
}

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

// Setup creates the first administrator and logs it in. It needs the
// one-time setup code of the node.
func (s *Service) Setup(ctx context.Context, username, password, setupCode, ip, ua string) (token string, sess Session, err error) {
	if err := domain.ValidateUsername("username", username); err != nil {
		return "", Session{}, err
	}
	if err := domain.ValidatePanelPassword(password); err != nil {
		return "", Session{}, err
	}
	// Answer before hashing: every hash costs 64 MiB, and this endpoint
	// stays public after the setup (audit A1).
	if required, err := s.SetupRequired(ctx); err != nil {
		return "", Session{}, err
	} else if !required {
		return "", Session{}, ErrSetupDone
	}
	if err := s.login.allowIP(ip); err != nil {
		return "", Session{}, err
	}
	if err := s.checkSetupCode(ctx, setupCode); err != nil {
		return "", Session{}, err
	}
	var (
		hash    string
		hashErr error
	)
	if err := s.hashes.do(ctx, func() { hash, hashErr = hashPassword(password) }); err != nil {
		return "", Session{}, err
	}
	if hashErr != nil {
		return "", Session{}, hashErr
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
	s.setupMu.Lock()
	s.setupCode = ""
	_ = os.Remove(filepath.Join(s.opts.DataDir, setupCodeFile))
	s.setupMu.Unlock()
	s.audit(ctx, Actor{Type: "user", ID: u.ID, Name: u.Username, IP: ip}, "auth.setup", "user", u.ID, nil)
	return s.newSession(ctx, u, ip, ua)
}

// Login checks credentials and opens a session.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (string, Session, error) {
	key := strings.ToLower(username) + "|" + ip
	if until, locked := s.login.locked(key); locked {
		return "", Session{}, &LockedError{Until: until}
	}
	// Every attempt from an address counts, whatever the username: a lock
	// per account alone lets a client try any number of accounts.
	if err := s.login.allowIP(ip); err != nil {
		return "", Session{}, err
	}
	row, err := s.store.R().GetUserByUsername(ctx, username)
	encoded := row.PasswordHash
	if err != nil {
		// Spend the same time as a real check.
		encoded = ""
	}
	var ok bool
	if herr := s.hashes.do(ctx, func() {
		if encoded == "" {
			encoded = dummyHash()
		}
		ok = verifyPassword(encoded, password)
	}); herr != nil {
		return "", Session{}, herr
	}
	if err != nil {
		s.login.fail(key)
		return "", Session{}, ErrUnauthenticated
	}
	if !ok || store.Bool(row.Disabled) {
		s.login.fail(key)
		s.audit(ctx, Actor{Type: "user", Name: username, IP: ip}, "auth.login_failed", "user", row.ID, nil)
		return "", Session{}, ErrUnauthenticated
	}
	s.login.success(key, ip)
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

// CheckSession reports whether a session token is still valid, without
// extending it: open WebSockets are checked with it, so a session that
// ends stops receiving live updates (audit M4).
func (s *Service) CheckSession(ctx context.Context, token string) error {
	if token == "" || len(token) > 100 {
		return ErrUnauthenticated
	}
	row, err := s.store.R().GetSession(ctx, tokenID(token))
	if err != nil || time.Now().After(store.Time(row.ExpiresAt)) || store.Bool(row.Disabled) {
		return ErrUnauthenticated
	}
	return nil
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

// hashLimiter bounds the Argon2id computations running at once. Each one
// takes 64 MiB, so unbounded concurrent logins could exhaust the node's
// memory and take every camera down with the service (audit A1). A few
// wait for a slot; the rest are refused at once.
type hashLimiter struct {
	slots   chan struct{}
	pending atomic.Int32
	wait    time.Duration
}

// Limits of concurrent password hashing.
const (
	maxConcurrentHashes = 2
	maxPendingHashes    = 16
	maxHashWait         = 5 * time.Second
)

// ErrBusy is returned when too many logins are being checked at once.
var ErrBusy = errors.New("too many sign-in attempts are being checked; try again in a few seconds")

func newHashLimiter() *hashLimiter {
	return &hashLimiter{slots: make(chan struct{}, maxConcurrentHashes), wait: maxHashWait}
}

// do runs fn when a slot is free, or fails with ErrBusy.
func (h *hashLimiter) do(ctx context.Context, fn func()) error {
	if h.pending.Add(1) > maxPendingHashes {
		h.pending.Add(-1)
		return ErrBusy
	}
	defer h.pending.Add(-1)
	timer := time.NewTimer(h.wait)
	defer timer.Stop()
	select {
	case h.slots <- struct{}{}:
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-h.slots }()
	fn()
	return nil
}

// RateLimitError is returned when an address makes too many sign-in
// attempts, whatever the accounts it tries.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("too many sign-in attempts from this address; try again in %s", e.RetryAfter.Round(time.Second))
}

// Limits of the login guard.
const (
	// maxGuardEntries bounds the memory of the guard. When it is full the
	// entries that matter least go first; it is never emptied, so flooding
	// it cannot clear the lock of an account under attack (audit M3).
	maxGuardEntries = 10_000
	// ipBurst attempts per address, refilled at ipRate.
	ipBurst = 10
	ipRate  = 10.0 / 60 // tokens per second
)

// loginGuard locks a username and IP pair after repeated failures, with a
// growing lock time, and limits the attempts of each address.
type loginGuard struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
	ips     map[string]*ipBucket
	now     func() time.Time
}

type loginEntry struct {
	failures int
	until    time.Time
	last     time.Time
}

type ipBucket struct {
	tokens float64
	at     time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{entries: map[string]*loginEntry{}, ips: map[string]*ipBucket{}, now: time.Now}
}

func (g *loginGuard) locked(key string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[key]
	if e == nil || g.now().After(e.until) {
		return time.Time{}, false
	}
	return e.until, true
}

func (g *loginGuard) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	e := g.entries[key]
	if e == nil {
		if len(g.entries) >= maxGuardEntries {
			g.evictEntries(now)
		}
		e = &loginEntry{}
		g.entries[key] = e
	}
	e.failures++
	e.last = now
	if e.failures >= 5 {
		lock := 30 * time.Second << min(e.failures-5, 5)
		if lock > 15*time.Minute {
			lock = 15 * time.Minute
		}
		e.until = now.Add(lock)
	}
}

// evictEntries frees a tenth of the guard: unlocked entries first, oldest
// failure first, and locked ones only when nothing else is left.
func (g *loginGuard) evictEntries(now time.Time) {
	type cand struct {
		key    string
		locked bool
		last   time.Time
	}
	list := make([]cand, 0, len(g.entries))
	for k, e := range g.entries {
		list = append(list, cand{k, now.Before(e.until), e.last})
	}
	slices.SortFunc(list, func(a, b cand) int {
		if a.locked != b.locked {
			if a.locked {
				return 1
			}
			return -1
		}
		return a.last.Compare(b.last)
	})
	for _, c := range list[:max(len(list)/10, 1)] {
		delete(g.entries, c.key)
	}
}

// ipKey groups IPv6 clients by their /64, which one host usually holds
// whole.
func ipKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// allowIP takes one attempt from the address's bucket.
func (g *loginGuard) allowIP(ip string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	key := ipKey(ip)
	b := g.ips[key]
	if b == nil {
		if len(g.ips) >= maxGuardEntries {
			g.evictBuckets(now)
		}
		b = &ipBucket{tokens: ipBurst, at: now}
		g.ips[key] = b
	}
	b.tokens = min(ipBurst, b.tokens+now.Sub(b.at).Seconds()*ipRate)
	b.at = now
	if b.tokens < 1 {
		return &RateLimitError{RetryAfter: time.Duration((1 - b.tokens) / ipRate * float64(time.Second))}
	}
	b.tokens--
	return nil
}

// evictBuckets drops full buckets, which hold nothing worth keeping, and
// then the least recently used ones.
func (g *loginGuard) evictBuckets(now time.Time) {
	for k, b := range g.ips {
		if b.tokens+now.Sub(b.at).Seconds()*ipRate >= ipBurst {
			delete(g.ips, k)
		}
	}
	if len(g.ips) < maxGuardEntries {
		return
	}
	keys := make([]string, 0, len(g.ips))
	for k := range g.ips {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return g.ips[a].at.Compare(g.ips[b].at) })
	for _, k := range keys[:max(len(keys)/10, 1)] {
		delete(g.ips, k)
	}
}

// success forgets the failures of a pair and gives the address its attempt
// back: a person who signs in is not the one guessing.
func (g *loginGuard) success(key, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, key)
	if b := g.ips[ipKey(ip)]; b != nil {
		b.tokens = min(ipBurst, b.tokens+1)
	}
}

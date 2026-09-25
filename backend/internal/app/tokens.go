package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// API token scopes (D54, D64): read allows safe requests and the WebSocket;
// write allows every request except managing tokens.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// tokenPrefix marks MockVision tokens, so secret scanners and people can
// recognize one.
const tokenPrefix = "mvt_"

// MaxTokenDays bounds the lifetime of a token that expires.
const MaxTokenDays = 3650

// ErrInvalidToken is returned for unknown, expired or revoked tokens.
var ErrInvalidToken = errors.New("the API token is invalid, expired or revoked")

// TokenInfo is the API token behind a request.
type TokenInfo struct {
	ID        string
	Name      string
	Scopes    []string
	ExpiresAt time.Time
}

// CanWrite reports whether the token may change state.
func (t *TokenInfo) CanWrite() bool { return slices.Contains(t.Scopes, ScopeWrite) }

// TokenView is an API token as the API returns it; the secret is shown only
// once, when the token is created.
type TokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip"`
	Expired    bool       `json:"expired"`
}

// TokenInput creates a token. No expiry means it lasts until revoked.
type TokenInput struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays *int     `json:"expires_in_days"`
}

// CreatedToken carries the secret of a new token.
type CreatedToken struct {
	Token  TokenView `json:"token"`
	Secret string    `json:"secret"`
}

// ListTokens returns the tokens of a user, newest first.
func (s *Service) ListTokens(ctx context.Context, userID string) ([]TokenView, error) {
	rows, err := s.store.R().ListAPITokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]TokenView, 0, len(rows))
	for _, r := range rows {
		out = append(out, tokenView(r))
	}
	return out, nil
}

// CreateToken issues a token for the actor's user.
func (s *Service) CreateToken(ctx context.Context, actor Actor, in TokenInput) (*CreatedToken, error) {
	if actor.ID == "" {
		return nil, ErrUnauthenticated
	}
	v := &domain.ValidationError{}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		v.Add("name", "is required")
	case utf8.RuneCountInString(in.Name) > 64:
		v.Add("name", "must be at most 64 characters")
	case strings.IndexFunc(in.Name, unicode.IsControl) >= 0:
		v.Add("name", "must not contain control characters")
	}
	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		v.Add("scopes", "%v", err)
	}
	var expires time.Time
	now := time.Now()
	if in.ExpiresInDays != nil {
		if *in.ExpiresInDays < 1 || *in.ExpiresInDays > MaxTokenDays {
			v.Add("expires_in_days", "must be between 1 and %d, or empty for a token that does not expire", MaxTokenDays)
		} else {
			expires = now.AddDate(0, 0, *in.ExpiresInDays)
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	scopesJSON, _ := json.Marshal(scopes)
	row := db.ApiToken{
		ID: ulid.Make().String(), UserID: actor.ID, Name: in.Name, TokenHash: tokenID(secret), Prefix: secret[:len(tokenPrefix)+8],
		ScopesJson: string(scopesJSON), ExpiresAt: store.NullMillis(expires), CreatedAt: now.UnixMilli(),
	}
	err = s.store.W().CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID: row.ID, UserID: row.UserID, Name: row.Name, TokenHash: row.TokenHash, Prefix: row.Prefix,
		ScopesJson: row.ScopesJson, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "you already have a token named %q", in.Name)
		}
		return nil, err
	}
	diff := map[string]any{"name": row.Name, "scopes": scopes, "prefix": row.Prefix}
	if !expires.IsZero() {
		diff["expires_at"] = expires.UTC()
	}
	s.audit(ctx, actor, "token.create", "token", row.ID, diff)
	return &CreatedToken{Token: tokenView(row), Secret: secret}, nil
}

// RevokeToken deletes a token of the actor's user; requests that carry it
// fail from then on.
func (s *Service) RevokeToken(ctx context.Context, actor Actor, id string) error {
	row, err := s.store.R().GetAPIToken(ctx, db.GetAPITokenParams{ID: id, UserID: actor.ID})
	if err != nil {
		return store.NotFound(err)
	}
	n, err := s.store.W().DeleteAPIToken(ctx, db.DeleteAPITokenParams{ID: id, UserID: actor.ID})
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.audit(ctx, actor, "token.revoke", "token", id, map[string]any{"name": row.Name, "prefix": row.Prefix})
	return nil
}

// AuthenticateToken resolves a Bearer token. The last use is recorded at
// most once a minute, to spare writes on SD cards.
func (s *Service) AuthenticateToken(ctx context.Context, secret, ip string) (Session, error) {
	if !strings.HasPrefix(secret, tokenPrefix) || len(secret) > 100 {
		return Session{}, ErrInvalidToken
	}
	row, err := s.store.R().GetAPITokenByHash(ctx, tokenID(secret))
	if err != nil {
		return Session{}, ErrInvalidToken
	}
	now := time.Now()
	expires := store.NullTime(row.ExpiresAt)
	if (!expires.IsZero() && now.After(expires)) || store.Bool(row.Disabled) {
		return Session{}, ErrInvalidToken
	}
	if last := store.NullTime(row.LastUsedAt); last.IsZero() || now.Sub(last) > time.Minute {
		_ = s.store.W().TouchAPIToken(ctx, db.TouchAPITokenParams{ID: row.ID, LastUsedAt: store.NullMillis(now), LastUsedIp: ip})
	}
	var scopes []string
	_ = json.Unmarshal([]byte(row.ScopesJson), &scopes)
	return Session{
		User:  domain.User{ID: row.UserID, Username: row.Username, Role: domain.Role(row.Role)},
		Token: &TokenInfo{ID: row.ID, Name: row.Name, Scopes: scopes, ExpiresAt: expires},
	}, nil
}

// normalizeScopes validates scopes; write implies read, and none means read.
func normalizeScopes(in []string) ([]string, error) {
	write := false
	for _, sc := range in {
		switch sc {
		case ScopeRead:
		case ScopeWrite:
			write = true
		default:
			return nil, errors.New("unknown scope " + sc + "; use read or write")
		}
	}
	if write {
		return []string{ScopeRead, ScopeWrite}, nil
	}
	return []string{ScopeRead}, nil
}

func tokenView(r db.ApiToken) TokenView {
	v := TokenView{
		ID: r.ID, Name: r.Name, Prefix: r.Prefix, Scopes: []string{}, CreatedAt: store.Time(r.CreatedAt), LastUsedIP: r.LastUsedIp,
	}
	_ = json.Unmarshal([]byte(r.ScopesJson), &v.Scopes)
	if t := store.NullTime(r.ExpiresAt); !t.IsZero() {
		v.ExpiresAt = &t
		v.Expired = time.Now().After(t)
	}
	if t := store.NullTime(r.LastUsedAt); !t.IsZero() {
		v.LastUsedAt = &t
	}
	return v
}

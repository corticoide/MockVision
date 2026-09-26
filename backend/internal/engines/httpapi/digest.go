package httpapi

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// Auth schemes.
const (
	SchemeDigest = "digest"
	SchemeBasic  = "basic"
	SchemeNone   = "none"
)

// nonceTTL bounds how long a digest nonce is accepted before the client is
// asked to retry with a fresh one (stale=true).
const nonceTTL = 5 * time.Minute

// authenticator checks HTTP Basic or Digest (RFC 7616, MD5, qop=auth)
// credentials against the camera's users. Nonces are stateless: a timestamp
// signed with a per-process key.
type authenticator struct {
	scheme string
	realm  string
	key    []byte
	opaque string
	users  func() []engine.User
	now    func() time.Time
	seen   replayCache
}

// Limits of the replay cache.
const (
	maxNonces       = 4096
	maxUsesPerNonce = 1024
)

// replayCache remembers the nc and cnonce each nonce was used with, so a
// Digest header captured on the network cannot be sent again (audit B6).
type replayCache struct {
	mu    sync.Mutex
	nonce map[string]*nonceUses
}

type nonceUses struct {
	first time.Time
	used  map[string]bool
}

// use records a use of a nonce; fresh is false for a use already seen,
// and full when the nonce served too many requests and must be renewed.
func (c *replayCache) use(nonce, nc, cnonce string, now time.Time) (fresh, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nonce == nil {
		c.nonce = map[string]*nonceUses{}
	}
	u := c.nonce[nonce]
	if u == nil {
		if len(c.nonce) >= maxNonces {
			c.prune(now)
		}
		u = &nonceUses{first: now, used: map[string]bool{}}
		c.nonce[nonce] = u
	}
	key := nc + ":" + cnonce
	switch {
	case u.used[key]:
		return false, false
	case len(u.used) >= maxUsesPerNonce:
		return false, true
	}
	u.used[key] = true
	return true, false
}

// prune forgets expired nonces, and the oldest ones when that is not
// enough; a forgotten nonce is expired or about to be.
func (c *replayCache) prune(now time.Time) {
	for n, u := range c.nonce {
		if now.Sub(u.first) > nonceTTL {
			delete(c.nonce, n)
		}
	}
	for len(c.nonce) >= maxNonces {
		oldest, at := "", now
		for n, u := range c.nonce {
			if !u.first.After(at) {
				oldest, at = n, u.first
			}
		}
		delete(c.nonce, oldest)
	}
}

func newAuthenticator(scheme, realm string, users func() []engine.User) *authenticator {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	op := make([]byte, 12)
	_, _ = rand.Read(op)
	if realm == "" {
		realm = "IPCamera"
	}
	return &authenticator{
		scheme: scheme,
		realm:  realm,
		key:    key,
		opaque: hex.EncodeToString(op),
		users:  users,
		now:    time.Now,
	}
}

// anonymous is the account of requests to a camera without authentication:
// the profile chose to let anyone do anything.
var anonymous = engine.User{Username: "anonymous", Role: RoleAdmin}

// check returns the authenticated account, or ok false and whether the
// nonce was merely stale.
func (a *authenticator) check(r *http.Request) (user engine.User, ok, stale bool) {
	h := r.Header.Get("Authorization")
	var name string
	switch {
	case a.scheme == SchemeNone:
		return anonymous, true, false
	case a.scheme == SchemeBasic && len(h) > 6 && strings.EqualFold(h[:6], "basic "):
		name = a.checkBasic(h[6:])
	case a.scheme == SchemeDigest && len(h) > 7 && strings.EqualFold(h[:7], "digest "):
		name, stale = a.checkDigest(r, h[7:])
	}
	if name == "" {
		return engine.User{}, false, stale
	}
	u, found := a.lookup(name)
	return u, found, false
}

// challenge writes the 401 response headers.
func (a *authenticator) challenge(w http.ResponseWriter, stale bool) {
	switch a.scheme {
	case SchemeBasic:
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q`, a.realm))
	case SchemeDigest:
		v := fmt.Sprintf(`Digest realm=%q, qop="auth", nonce=%q, opaque=%q, algorithm=MD5`, a.realm, a.nonce(), a.opaque)
		if stale {
			v += ", stale=true"
		}
		w.Header().Set("WWW-Authenticate", v)
	}
}

func (a *authenticator) lookup(name string) (engine.User, bool) {
	for _, u := range a.users() {
		if u.Username == name {
			return u, true
		}
	}
	return engine.User{}, false
}

func (a *authenticator) checkBasic(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return ""
	}
	name, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return ""
	}
	u, found := a.lookup(name)
	if !found || subtle.ConstantTimeCompare([]byte(u.Password), []byte(pass)) != 1 {
		return ""
	}
	return name
}

func (a *authenticator) nonce() string {
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(a.now().UnixNano()))
	mac := hmac.New(sha256.New, a.key)
	mac.Write(ts[:])
	return base64.RawURLEncoding.EncodeToString(append(ts[:], mac.Sum(nil)[:16]...))
}

// validNonce reports whether the nonce is ours and whether it expired.
func (a *authenticator) validNonce(n string) (ok, expired bool) {
	raw, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil || len(raw) != 24 {
		return false, false
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write(raw[:8])
	if !hmac.Equal(mac.Sum(nil)[:16], raw[8:]) {
		return false, false
	}
	issued := time.Unix(0, int64(binary.BigEndian.Uint64(raw[:8])))
	return true, a.now().Sub(issued) > nonceTTL
}

func (a *authenticator) checkDigest(r *http.Request, header string) (string, bool) {
	p := parseDigest(header)
	name := p["username"]
	u, found := a.lookup(name)
	if !found || p["realm"] != a.realm {
		return "", false
	}
	ok, expired := a.validNonce(p["nonce"])
	if !ok {
		return "", false
	}
	// The digest covers the uri the client sent: it must be the whole
	// request target, query included, or a header signed for one request
	// would authorize another (audit B6).
	if p["uri"] != r.RequestURI {
		return "", false
	}
	if alg := p["algorithm"]; alg != "" && !strings.EqualFold(alg, "MD5") {
		return "", false
	}
	ha1 := md5hex(name + ":" + a.realm + ":" + u.Password)
	ha2 := md5hex(r.Method + ":" + p["uri"])
	var want string
	switch p["qop"] {
	case "auth":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	case "":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	default:
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(p["response"]))) != 1 {
		return "", false
	}
	if expired {
		return "", true
	}
	// RFC 2069 clients (no qop) reuse the nonce as is: only qop=auth
	// requests carry the counter that tells a replay.
	if p["qop"] == "auth" {
		if fresh, full := a.seen.use(p["nonce"], p["nc"], p["cnonce"], a.now()); !fresh {
			return "", full
		}
	}
	return name, false
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseDigest parses the comma separated key=value list of a Digest
// Authorization header, with optional quotes.
func parseDigest(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			s = s[1:]
			var b strings.Builder
			i := 0
			for ; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if s[i] == '"' {
					break
				}
				b.WriteByte(s[i])
			}
			val = b.String()
			if i < len(s) {
				s = s[i+1:]
			} else {
				s = ""
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		out[key] = val
	}
	return out
}

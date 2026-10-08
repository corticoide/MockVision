// Package digest answers HTTP Digest challenges (RFC 7616) as a client:
// http-push toward targets that ask for it, and the self-test toward the
// camera it checks.
package digest

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// Challenge is a Digest challenge of a server (RFC 7616).
type Challenge struct {
	realm     string
	nonce     string
	opaque    string
	algorithm string
	qop       string // auth, auth-int or "" (RFC 2069)
	userhash  bool
}

// ParseChallenge picks the Digest challenge among a 401's WWW-Authenticate
// headers, preferring SHA-256 over MD5 when the server offers both.
func ParseChallenge(headers []string) (*Challenge, bool) {
	var best *Challenge
	for _, h := range headers {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
		if !strings.EqualFold(scheme, "Digest") {
			continue
		}
		p := parseParams(rest)
		c := &Challenge{realm: p["realm"], nonce: p["nonce"], opaque: p["opaque"], algorithm: strings.ToUpper(p["algorithm"]), userhash: strings.EqualFold(p["userhash"], "true")}
		if c.algorithm == "" {
			c.algorithm = "MD5"
		}
		if _, ok := hashes[strings.TrimSuffix(c.algorithm, "-SESS")]; !ok || c.nonce == "" {
			continue
		}
		for _, q := range strings.Split(p["qop"], ",") {
			switch strings.TrimSpace(q) {
			case "auth":
				c.qop = "auth"
			case "auth-int":
				if c.qop == "" {
					c.qop = "auth-int"
				}
			}
		}
		if best == nil || (strings.HasPrefix(c.algorithm, "SHA-256") && !strings.HasPrefix(best.algorithm, "SHA-256")) {
			best = c
		}
	}
	return best, best != nil
}

var hashes = map[string]func() hash.Hash{"MD5": md5.New, "SHA-256": sha256.New}

func hexHash(newHash func() hash.Hash, parts ...string) string {
	h := newHash()
	h.Write([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(h.Sum(nil))
}

// Authorize builds the Authorization header answering the challenge.
func (c *Challenge) Authorize(method, uri, user, password string, body []byte) (string, error) {
	newHash := hashes[strings.TrimSuffix(c.algorithm, "-SESS")]
	cn := make([]byte, 12)
	if _, err := rand.Read(cn); err != nil {
		return "", err
	}
	cnonce := hex.EncodeToString(cn)
	const nc = "00000001"
	ha1 := hexHash(newHash, user, c.realm, password)
	if strings.HasSuffix(c.algorithm, "-SESS") {
		ha1 = hexHash(newHash, ha1, c.nonce, cnonce)
	}
	ha2 := hexHash(newHash, method, uri)
	if c.qop == "auth-int" {
		ha2 = hexHash(newHash, method, uri, hexHash(newHash, string(body)))
	}
	var response string
	if c.qop == "" {
		response = hexHash(newHash, ha1, c.nonce, ha2)
	} else {
		response = hexHash(newHash, ha1, c.nonce, nc, cnonce, c.qop, ha2)
	}
	username := user
	if c.userhash {
		username = hexHash(newHash, user, c.realm)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%s, realm=%s, nonce=%s, uri=%s, algorithm=%s, response="%s"`,
		quote(username), quote(c.realm), quote(c.nonce), quote(uri), c.algorithm, response)
	if c.qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, c.qop, nc, cnonce)
	}
	if c.opaque != "" {
		fmt.Fprintf(&b, `, opaque=%s`, quote(c.opaque))
	}
	if c.userhash {
		b.WriteString(", userhash=true")
	}
	return b.String(), nil
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// parseParams parses the comma separated key=value pairs of a challenge;
// values may be quoted, with backslash escapes.
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t,")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
			val, s = b.String(), s[min(i+1, len(s)):]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val, s = strings.TrimSpace(s[:end]), s[end:]
		}
		out[key] = val
	}
	return out
}

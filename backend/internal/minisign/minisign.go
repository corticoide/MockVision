// Package minisign reads and writes Ed25519 keys and signatures in the
// format of minisign (https://jedisct1.github.io/minisign/), so packages
// can be signed and verified offline with MockVision or with minisign
// itself (D83).
//
// A public key is "Ed", an 8-byte key ID and the 32-byte Ed25519 key,
// base64-encoded under an untrusted comment line. A signature file holds
// the signature of the message's BLAKE2b-512 hash ("ED"; "Ed", over the
// message itself, is read too), then a trusted comment and a global
// signature over the signature and that comment, so the comment cannot be
// changed either. A secret key may be encrypted with scrypt, with
// libsodium's parameters.
package minisign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/scrypt"
)

const (
	algLegacy  = "Ed" // signature of the message
	algHashed  = "ED" // signature of its BLAKE2b-512 hash
	kdfScrypt  = "Sc"
	kdfNone    = "\x00\x00"
	chkBlake2b = "B2"

	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "
)

// Default scrypt limits of a new secret key: minisign's "sensitive" ones
// would need 1 GiB of memory to sign; these need 16 MiB, as libsodium's
// interactive limits.
const (
	DefaultOpsLimit = 1 << 19
	DefaultMemLimit = 1 << 24
)

// ErrInvalidSignature is a signature that does not verify.
var ErrInvalidSignature = errors.New("minisign: signature verification failed")

// PublicKey is an Ed25519 public key with minisign's key ID.
type PublicKey struct {
	ID  uint64
	Key ed25519.PublicKey
}

// KeyID formats a key ID as minisign prints it.
func KeyID(id uint64) string { return fmt.Sprintf("%016X", id) }

// String returns the key's base64 line, what minisign -P takes.
func (p PublicKey) String() string {
	b := make([]byte, 0, 42)
	b = append(b, algLegacy...)
	b = binary.LittleEndian.AppendUint64(b, p.ID)
	b = append(b, p.Key...)
	return base64.StdEncoding.EncodeToString(b)
}

// File returns the key as a .pub file.
func (p PublicKey) File() []byte {
	return []byte(untrustedPrefix + "minisign public key " + KeyID(p.ID) + "\n" + p.String() + "\n")
}

// ParsePublicKey reads a public key: its base64 line alone or a .pub file.
func ParsePublicKey(text string) (PublicKey, error) {
	line := strings.TrimSpace(text)
	if strings.HasPrefix(line, untrustedPrefix) {
		_, rest, ok := strings.Cut(line, "\n")
		if !ok {
			return PublicKey{}, errors.New("minisign: the public key file has no key")
		}
		line = strings.TrimSpace(rest)
	}
	if strings.ContainsAny(line, "\r\n") {
		return PublicKey{}, errors.New("minisign: a public key is one line")
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(raw) != 42 {
		return PublicKey{}, errors.New("minisign: not a public key")
	}
	if string(raw[:2]) != algLegacy {
		return PublicKey{}, fmt.Errorf("minisign: unsupported key algorithm %q", raw[:2])
	}
	return PublicKey{ID: binary.LittleEndian.Uint64(raw[2:10]), Key: ed25519.PublicKey(bytes.Clone(raw[10:]))}, nil
}

// Signature is a parsed signature file.
type Signature struct {
	Algorithm        string
	KeyID            uint64
	Sig              []byte
	UntrustedComment string
	TrustedComment   string
	GlobalSig        []byte
}

// ParseSignature reads a signature file (.minisig).
func ParseSignature(data []byte) (Signature, error) {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	if len(lines) != 4 {
		return Signature{}, errors.New("minisign: a signature file has four lines")
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) || !strings.HasPrefix(lines[2], trustedPrefix) {
		return Signature{}, errors.New("minisign: malformed signature file")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != 74 {
		return Signature{}, errors.New("minisign: malformed signature")
	}
	alg := string(raw[:2])
	if alg != algHashed && alg != algLegacy {
		return Signature{}, fmt.Errorf("minisign: unsupported signature algorithm %q", alg)
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return Signature{}, errors.New("minisign: malformed global signature")
	}
	return Signature{
		Algorithm: alg, KeyID: binary.LittleEndian.Uint64(raw[2:10]), Sig: raw[10:],
		UntrustedComment: strings.TrimPrefix(lines[0], untrustedPrefix),
		TrustedComment:   strings.TrimPrefix(lines[2], trustedPrefix), GlobalSig: global,
	}, nil
}

// Verify checks a signature of msg with a public key: the key ID, the
// signature and the global signature over the trusted comment.
func Verify(pub PublicKey, msg []byte, sig Signature) error {
	if sig.KeyID != pub.ID {
		return fmt.Errorf("minisign: signed with key %s, not %s", KeyID(sig.KeyID), KeyID(pub.ID))
	}
	signed := msg
	if sig.Algorithm == algHashed {
		h := blake2b.Sum512(msg)
		signed = h[:]
	}
	if !ed25519.Verify(pub.Key, signed, sig.Sig) {
		return ErrInvalidSignature
	}
	if !ed25519.Verify(pub.Key, append(bytes.Clone(sig.Sig), sig.TrustedComment...), sig.GlobalSig) {
		return ErrInvalidSignature
	}
	return nil
}

// PrivateKey is an Ed25519 secret key with its key ID.
type PrivateKey struct {
	ID  uint64
	Key ed25519.PrivateKey
}

// Public returns the key's public half.
func (k PrivateKey) Public() PublicKey {
	return PublicKey{ID: k.ID, Key: k.Key.Public().(ed25519.PublicKey)}
}

// GenerateKey creates a key pair with a random key ID.
func GenerateKey(r io.Reader) (PrivateKey, error) {
	if r == nil {
		r = rand.Reader
	}
	_, sk, err := ed25519.GenerateKey(r)
	if err != nil {
		return PrivateKey{}, err
	}
	var id [8]byte
	if _, err := io.ReadFull(r, id[:]); err != nil {
		return PrivateKey{}, err
	}
	return PrivateKey{ID: binary.LittleEndian.Uint64(id[:]), Key: sk}, nil
}

// Sign signs msg as minisign does by default: the BLAKE2b-512 hash, with a
// trusted comment carrying the time and the file name unless given.
func Sign(k PrivateKey, msg []byte, trustedComment, untrustedComment string) []byte {
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(k.Key, h[:])
	if strings.ContainsAny(trustedComment, "\r\n") {
		trustedComment = strings.NewReplacer("\r", " ", "\n", " ").Replace(trustedComment)
	}
	if trustedComment == "" {
		trustedComment = fmt.Sprintf("timestamp:%d", time.Now().Unix())
	}
	if untrustedComment == "" {
		untrustedComment = "signature from minisign secret key"
	}
	global := ed25519.Sign(k.Key, append(bytes.Clone(sig), trustedComment...))
	raw := make([]byte, 0, 74)
	raw = append(raw, algHashed...)
	raw = binary.LittleEndian.AppendUint64(raw, k.ID)
	raw = append(raw, sig...)
	var b strings.Builder
	b.WriteString(untrustedPrefix + strings.NewReplacer("\r", " ", "\n", " ").Replace(untrustedComment) + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
	b.WriteString(trustedPrefix + trustedComment + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(global) + "\n")
	return []byte(b.String())
}

// secretKeySize is the decoded size of a secret key file: algorithms,
// KDF salt and limits, then the key ID, the key and its checksum.
const secretKeySize = 2 + 2 + 2 + 32 + 8 + 8 + 8 + 64 + 32

// MarshalPrivateKey writes a secret key file, encrypted with password
// unless it is empty (minisign -W).
func MarshalPrivateKey(k PrivateKey, password []byte, opsLimit, memLimit uint64) ([]byte, error) {
	raw := make([]byte, 0, secretKeySize)
	raw = append(raw, algLegacy...)
	if len(password) > 0 {
		raw = append(raw, kdfScrypt...)
	} else {
		raw = append(raw, kdfNone...)
	}
	raw = append(raw, chkBlake2b...)
	salt := make([]byte, 32)
	if len(password) > 0 {
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
	} else {
		opsLimit, memLimit = 0, 0
	}
	raw = append(raw, salt...)
	raw = binary.LittleEndian.AppendUint64(raw, opsLimit)
	raw = binary.LittleEndian.AppendUint64(raw, memLimit)
	keynum := make([]byte, 0, 104)
	keynum = binary.LittleEndian.AppendUint64(keynum, k.ID)
	keynum = append(keynum, k.Key...)
	keynum = append(keynum, checksum(k.ID, k.Key)...)
	if len(password) > 0 {
		stream, err := kdf(password, salt, opsLimit, memLimit)
		if err != nil {
			return nil, err
		}
		subtle.XORBytes(keynum, keynum, stream)
	}
	raw = append(raw, keynum...)
	comment := "minisign secret key"
	if len(password) > 0 {
		comment = "minisign encrypted secret key"
	}
	return []byte(untrustedPrefix + comment + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"), nil
}

// ParsePrivateKey reads a secret key file, decrypting it with password
// when it is encrypted.
func ParsePrivateKey(data, password []byte) (PrivateKey, error) {
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, untrustedPrefix) {
		_, rest, ok := strings.Cut(text, "\n")
		if !ok {
			return PrivateKey{}, errors.New("minisign: the secret key file has no key")
		}
		text = strings.TrimSpace(rest)
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(raw) != secretKeySize {
		return PrivateKey{}, errors.New("minisign: not a secret key")
	}
	if string(raw[:2]) != algLegacy || string(raw[4:6]) != chkBlake2b {
		return PrivateKey{}, errors.New("minisign: unsupported secret key algorithms")
	}
	salt := raw[6:38]
	ops := binary.LittleEndian.Uint64(raw[38:46])
	mem := binary.LittleEndian.Uint64(raw[46:54])
	keynum := bytes.Clone(raw[54:])
	switch string(raw[2:4]) {
	case kdfScrypt:
		if len(password) == 0 {
			return PrivateKey{}, errors.New("minisign: the secret key is encrypted; a password is needed")
		}
		stream, err := kdf(password, salt, ops, mem)
		if err != nil {
			return PrivateKey{}, err
		}
		subtle.XORBytes(keynum, keynum, stream)
	case kdfNone:
	default:
		return PrivateKey{}, errors.New("minisign: unsupported key derivation")
	}
	id := binary.LittleEndian.Uint64(keynum[:8])
	sk := ed25519.PrivateKey(keynum[8:72])
	// minisign leaves the checksum of an unencrypted key empty; the key
	// then proves itself, its public half derived again from its seed.
	if string(raw[2:4]) == kdfScrypt && subtle.ConstantTimeCompare(checksum(id, sk), keynum[72:]) != 1 {
		return PrivateKey{}, errors.New("minisign: wrong password or corrupt secret key")
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(sk.Seed()), sk) {
		return PrivateKey{}, errors.New("minisign: corrupt secret key")
	}
	return PrivateKey{ID: id, Key: bytes.Clone(sk)}, nil
}

func checksum(id uint64, sk ed25519.PrivateKey) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte(algLegacy))
	_ = binary.Write(h, binary.LittleEndian, id)
	h.Write(sk)
	return h.Sum(nil)
}

// maxMemLimit bounds the memory a secret key may ask scrypt for.
const maxMemLimit = 2 << 30

// kdf derives the 104 bytes that encrypt a secret key, with the scrypt
// parameters libsodium's crypto_pwhash_scryptsalsa208sha256 picks from
// an operations and a memory limit.
func kdf(password, salt []byte, opsLimit, memLimit uint64) ([]byte, error) {
	if memLimit > maxMemLimit {
		return nil, fmt.Errorf("minisign: the secret key asks for %d MiB to decrypt", memLimit>>20)
	}
	if opsLimit < 32768 {
		opsLimit = 32768
	}
	const r = 8
	var nLog2, p uint64
	if opsLimit < memLimit/32 {
		p = 1
		maxN := opsLimit / (r * 4)
		for nLog2 = 1; nLog2 < 63; nLog2++ {
			if uint64(1)<<nLog2 > maxN/2 {
				break
			}
		}
	} else {
		maxN := memLimit / (r * 128)
		for nLog2 = 1; nLog2 < 63; nLog2++ {
			if uint64(1)<<nLog2 > maxN/2 {
				break
			}
		}
		maxrp := (opsLimit / 4) / (uint64(1) << nLog2)
		if maxrp > 0x3fffffff {
			maxrp = 0x3fffffff
		}
		p = maxrp / r
	}
	if nLog2 > 30 || p == 0 {
		return nil, errors.New("minisign: unusable key derivation limits")
	}
	return scrypt.Key(password, salt, 1<<nLog2, r, int(p), 104)
}

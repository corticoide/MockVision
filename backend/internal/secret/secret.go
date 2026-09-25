// Package secret encrypts credentials stored in the database (camera users,
// target passwords) with XChaCha20-Poly1305. The node key lives in a file
// outside the database, so a copy of db.sqlite alone reveals no secrets.
package secret

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

// Box seals and opens secrets with the node key.
type Box struct {
	aead cipher.AEAD
}

// LoadOrCreate reads the node key at path, creating it (mode 0600) when it
// does not exist.
func LoadOrCreate(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, chacha20poly1305.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create node key: %w", err)
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read node key: %w", err)
	}
	return New(key)
}

// New builds a Box from a 32 byte key.
func New(key []byte) (*Box, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("node key must be %d bytes, got %d", chacha20poly1305.KeySize, len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. The context (for example "camera_users:<id>")
// is authenticated, so a ciphertext cannot be moved to another row.
func (b *Box) Seal(plaintext []byte, context string) []byte {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic("secret: no randomness: " + err.Error())
	}
	return b.aead.Seal(nonce, nonce, plaintext, []byte(context))
}

// Open decrypts a value produced by Seal with the same context.
func (b *Box) Open(sealed []byte, context string) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < ns+b.aead.Overhead() {
		return nil, errors.New("secret: ciphertext too short")
	}
	plain, err := b.aead.Open(nil, sealed[:ns], sealed[ns:], []byte(context))
	if err != nil {
		return nil, errors.New("secret: cannot decrypt (wrong key or context)")
	}
	return plain, nil
}

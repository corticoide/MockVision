package minisign

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The test data comes from minisign 0.11: a key made with -W, a signature
// of the default kind (hashed) and a legacy one (-l).
func TestVerifyMinisignSignatures(t *testing.T) {
	pub, err := ParsePublicKey(string(read(t, "real.pub")))
	if err != nil {
		t.Fatal(err)
	}
	if KeyID(pub.ID) != "7720BE83E2F65DE1" {
		t.Fatalf("key id %s", KeyID(pub.ID))
	}
	msg := read(t, "msg.txt")
	for _, f := range []string{"msg.minisig", "msg-legacy.minisig"} {
		sig, err := ParseSignature(read(t, f))
		if err != nil {
			t.Fatal(f, err)
		}
		if err := Verify(pub, msg, sig); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if err := Verify(pub, append(bytes.Clone(msg), ' '), sig); err != ErrInvalidSignature {
			t.Fatalf("%s: a changed message verified: %v", f, err)
		}
	}
	sig, _ := ParseSignature(read(t, "msg.minisig"))
	if sig.TrustedComment != "file:manifest.yaml" {
		t.Fatalf("trusted comment %q", sig.TrustedComment)
	}
	// The trusted comment is signed too.
	forged := sig
	forged.TrustedComment = "file:other.yaml"
	if err := Verify(pub, msg, forged); err != ErrInvalidSignature {
		t.Fatalf("a changed trusted comment verified: %v", err)
	}
	// Another key does not verify it, and says whose it is.
	other, _ := GenerateKey(nil)
	if err := Verify(other.Public(), msg, sig); err == nil || !strings.Contains(err.Error(), "7720BE83E2F65DE1") {
		t.Fatalf("another key: %v", err)
	}
	// The base64 line alone is a key too.
	if again, err := ParsePublicKey(pub.String()); err != nil || again.ID != pub.ID || !bytes.Equal(again.Key, pub.Key) {
		t.Fatalf("base64 line: %v", err)
	}
}

// minisign's unencrypted secret key signs here, and what it signs
// verifies with its public key.
func TestSignWithMinisignKey(t *testing.T) {
	k, err := ParsePrivateKey(read(t, "real.key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublicKey(string(read(t, "real.pub")))
	if k.ID != pub.ID || !bytes.Equal(k.Public().Key, pub.Key) {
		t.Fatal("the secret key does not match its public key")
	}
	msg := []byte("manifest")
	sig, err := ParseSignature(Sign(k, msg, "file:manifest.yaml", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatal(err)
	}
}

// A new key round-trips, encrypted or not; a wrong password is refused.
func TestKeyRoundTrip(t *testing.T) {
	k, err := GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := MarshalPrivateKey(k, []byte("correct horse"), DefaultOpsLimit, DefaultMemLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(enc), "untrusted comment: minisign encrypted secret key\n") {
		t.Fatalf("file %q", enc)
	}
	if _, err := ParsePrivateKey(enc, nil); err == nil {
		t.Fatal("an encrypted key opened without its password")
	}
	if _, err := ParsePrivateKey(enc, []byte("wrong")); err == nil {
		t.Fatal("an encrypted key opened with a wrong password")
	}
	got, err := ParsePrivateKey(enc, []byte("correct horse"))
	if err != nil || got.ID != k.ID || !bytes.Equal(got.Key, k.Key) {
		t.Fatalf("decrypted %v", err)
	}
	plain, _ := MarshalPrivateKey(k, nil, 0, 0)
	if got, err := ParsePrivateKey(plain, nil); err != nil || !bytes.Equal(got.Key, k.Key) {
		t.Fatalf("plain %v", err)
	}
	pub, err := ParsePublicKey(string(k.Public().File()))
	if err != nil || pub.ID != k.ID {
		t.Fatalf("public %v", err)
	}
}

func TestMalformedInput(t *testing.T) {
	for _, s := range []string{"", "RWQ=", "untrusted comment: x", "not base64 !!"} {
		if _, err := ParsePublicKey(s); err == nil {
			t.Fatalf("public key %q accepted", s)
		}
	}
	good := string(read(t, "msg.minisig"))
	lines := strings.Split(good, "\n")
	for name, s := range map[string]string{
		"three lines":   strings.Join(lines[:3], "\n"),
		"no comment":    strings.Replace(good, "untrusted comment: ", "", 1),
		"short":         strings.Replace(good, lines[1], lines[1][:20], 1),
		"bad algorithm": strings.Replace(good, lines[1], "RXo"+lines[1][3:], 1),
	} {
		if _, err := ParseSignature([]byte(s)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

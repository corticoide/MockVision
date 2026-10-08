package pkg

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/minisign"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// TrustedKey is a key whose signatures the node trusts: the project's
// official catalog keys, built into the binary, and the ones an admin adds
// in Settings, such as a company's (D83).
type TrustedKey struct {
	Name string `json:"name"`
	// Key is the minisign public key, its base64 line.
	Key      string `json:"key"`
	Official bool   `json:"official,omitempty"`
}

// officialKeys are the project's catalog keys, comma-separated, stamped at
// release time with -ldflags "-X .../pkg.officialKeys=RWQ...". The catalog
// key is not the release key, and several can be listed so one can be
// rotated out. A development build has none: its catalog is unsigned.
var officialKeys = ""

// OfficialKeys returns the official catalog keys of this build.
func OfficialKeys() []TrustedKey {
	var out []TrustedKey
	for _, k := range strings.Split(officialKeys, ",") {
		k = strings.TrimSpace(k)
		if pub, err := minisign.ParsePublicKey(k); err == nil {
			out = append(out, TrustedKey{Name: "MockVision official catalog " + minisign.KeyID(pub.ID), Key: pub.String(), Official: true})
		}
	}
	return out
}

// signature checks manifest.sig against the trusted keys. An invalid
// signature rejects the package; a valid one by a key the node does not
// know proves nothing, so the package counts as unsigned (D84).
func (in *inspector) signature(files map[string][]byte) bool {
	raw, signed := files["manifest.sig"]
	if !signed {
		in.step(profile.StepSignature, "passed", "unsigned")
		in.warn(profile.StepSignature, "manifest.yaml", "the package is not signed: nothing proves who made it or that it is unchanged")
		return true
	}
	fail := func(format string, args ...any) bool {
		in.res.Report.Signature = SignatureInvalid
		in.problem(profile.StepSignature, "manifest.sig", 0, format, args...)
		in.step(profile.StepSignature, "failed", "")
		return false
	}
	sig, err := minisign.ParseSignature(raw)
	if err != nil {
		return fail("manifest.sig is not a minisign signature: %v", err)
	}
	keyID := minisign.KeyID(sig.KeyID)
	in.res.Report.KeyID = keyID
	for _, k := range in.opts.Keys {
		pub, err := minisign.ParsePublicKey(k.Key)
		if err != nil || pub.ID != sig.KeyID {
			continue
		}
		if err := minisign.Verify(pub, files["manifest.yaml"], sig); err != nil {
			return fail("the signature by %s (%s) does not match manifest.yaml: the package changed after it was signed", k.Name, keyID)
		}
		in.res.Report.Signature = SignatureTrusted
		if k.Official {
			in.res.Report.Signature = SignatureOfficial
		}
		in.res.Report.Signer = k.Name
		in.step(profile.StepSignature, "passed", fmt.Sprintf("signed by %s (%s)", k.Name, keyID))
		return true
	}
	in.step(profile.StepSignature, "passed", "signed by an unknown key")
	in.warn(profile.StepSignature, "manifest.sig", "signed with key %s, which this node does not trust: add it in Settings to trust its packages", keyID)
	return true
}

// Sign adds the signature of a package's manifest, replacing one it has.
// Every other file is copied as it is; the manifest lists their sha256, so
// signing it signs them all.
func Sign(data []byte, key minisign.PrivateKey) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a package: %w", err)
	}
	var manifest []byte
	for _, f := range zr.File {
		if f.Name != "manifest.yaml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		manifest, err = io.ReadAll(io.LimitReader(rc, MaxPackageBytes))
		rc.Close()
		if err != nil {
			return nil, err
		}
	}
	if manifest == nil {
		return nil, fmt.Errorf("the package has no manifest.yaml")
	}
	m, err := readManifest(manifest)
	if err != nil {
		return nil, err
	}
	comment := fmt.Sprintf("timestamp:%d\tpackage:%s@%s", time.Now().Unix(), m.ID, m.Version)
	sig := minisign.Sign(key, manifest, comment, "signature of a MockVision package")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if f.Name == "manifest.sig" {
			continue
		}
		if err := zw.Copy(f); err != nil {
			return nil, err
		}
		if f.Name == "manifest.yaml" {
			w, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.sig", Method: zip.Deflate})
			if err != nil {
				return nil, err
			}
			if _, err := w.Write(sig); err != nil {
				return nil, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

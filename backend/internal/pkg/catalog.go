package pkg

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/backend/internal/minisign"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// CatalogIndex is catalog/catalog.yaml: the profiles packaged as the
// official catalog, parents first.
const CatalogIndex = "catalog/catalog.yaml"

type catalogIndex struct {
	Packages []struct {
		Profile    string     `yaml:"profile"`
		License    string     `yaml:"license"`
		Provenance Provenance `yaml:"provenance"`
	} `yaml:"packages"`
}

// CatalogPackage is one package of the official catalog.
type CatalogPackage struct {
	ID      string
	Version string
	// Name is the file a release publishes it as.
	Name string
	Data []byte
	// Signed says whether a release signature came with it.
	Signed bool
}

// SignatureName is where a release keeps a catalog package's signature.
func SignatureName(id, version string) string {
	return "catalog/" + strings.ReplaceAll(id, "/", "-") + "-" + version + ".minisig"
}

// BuildCatalog packages the official catalog from the profiles directory.
// The same sources always give the same bytes, so the signatures a release
// made of their manifests still match.
func BuildCatalog(dir fs.FS) ([]CatalogPackage, error) {
	raw, err := fs.ReadFile(dir, CatalogIndex)
	if err != nil {
		return nil, err
	}
	var idx catalogIndex
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("%s: %w", CatalogIndex, err)
	}
	var out []CatalogPackage
	for _, e := range idx.Packages {
		p, err := catalogPackage(dir, e.Profile, e.License, e.Provenance)
		if err != nil {
			return nil, fmt.Errorf("catalog profile %s: %w", e.Profile, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func catalogPackage(dir fs.FS, file, license string, prov Provenance) (CatalogPackage, error) {
	data, err := fs.ReadFile(dir, path.Clean(file))
	if err != nil {
		return CatalogPackage{}, err
	}
	generic, _, err := profile.ParseYAML(data, profile.DefaultLimits)
	if err != nil {
		return CatalogPackage{}, err
	}
	root, _ := generic.(map[string]any)
	meta, _ := root["profile"].(map[string]any)
	id, _ := meta["id"].(string)
	version, _ := meta["version"].(string)
	if id == "" || version == "" {
		return CatalogPackage{}, errors.New("profile.id and profile.version are needed")
	}
	man := map[string]any{"format": FormatVersion, "kind": "profile", "id": id, "version": version,
		"requires": map[string]any{"profile_schema": profile.SchemaVersion}}
	if license != "" {
		man["license"] = license
	}
	provenance := map[string]any{}
	if prov.Source != "" {
		provenance["source"] = prov.Source
	}
	if len(prov.Firmware) > 0 {
		provenance["firmware"] = prov.Firmware
	}
	if prov.CapturedAt != "" {
		provenance["captured_at"] = prov.CapturedAt
	}
	if len(provenance) > 0 {
		man["provenance"] = provenance
	}
	pkgData, err := pack(man, map[string][]byte{"profile.yaml": data})
	if err != nil {
		return CatalogPackage{}, err
	}
	p := CatalogPackage{ID: id, Version: version, Name: strings.ReplaceAll(id, "/", "-") + "-" + version + ".mvpkg", Data: pkgData}
	if sig, err := fs.ReadFile(dir, SignatureName(id, version)); err == nil {
		if p.Data, err = withSignature(pkgData, sig); err != nil {
			return CatalogPackage{}, err
		}
		p.Signed = true
	}
	return p, nil
}

// withSignature adds manifest.sig to a package.
func withSignature(data, sig []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
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

// signCatalog signs each catalog package's manifest, for a release.
func signCatalog(dir fs.FS, key minisign.PrivateKey) (map[string][]byte, error) {
	pkgs, err := BuildCatalog(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, p := range pkgs {
		signed, err := Sign(p.Data, key)
		if err != nil {
			return nil, err
		}
		sig, err := fileOf(signed, "manifest.sig")
		if err != nil {
			return nil, err
		}
		out[SignatureName(p.ID, p.Version)] = sig
	}
	return out, nil
}

// fileOf reads one file of a package.
func fileOf(data []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, MaxPackageBytes))
		}
	}
	return nil, fmt.Errorf("the package has no %s", name)
}

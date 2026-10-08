package pkg

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/profile"
)

// FileName is the name a profile version's package is saved as.
func FileName(id, version string) string {
	return strings.ReplaceAll(id, "/", "-") + "-" + version + ".mvpkg"
}

// Pack builds a profile package from its files, with a manifest of the
// given identity and provenance.
func Pack(id, version string, prov Provenance, files map[string][]byte) ([]byte, error) {
	man := map[string]any{"format": FormatVersion, "kind": "profile", "id": id, "version": version,
		"requires": map[string]any{"profile_schema": profile.SchemaVersion}}
	if prov.Source != "" {
		man["provenance"] = map[string]any{"source": prov.Source}
	}
	return pack(man, files)
}

// WrapLoose wraps a loose profile.yaml in an unsigned draft package, as the
// importer took it.
func WrapLoose(data []byte, id, version string) ([]byte, error) {
	return Pack(id, version, Provenance{Source: "draft"}, map[string][]byte{"profile.yaml": data})
}

// Files reads every file of a package the node has accepted.
func Files(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxUnpackedBytes))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		out[f.Name] = b
	}
	return out, nil
}

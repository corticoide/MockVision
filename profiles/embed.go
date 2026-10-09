// Package profiles publishes the profile JSON Schema and the official
// profile catalog, both embedded in the binary.
package profiles

import "embed"

// Schema holds the published JSON Schema of profile.yaml.
//
//go:embed schema/profile.schema.json
var Schema []byte

// Catalog holds the official profiles shipped with each release: the
// profiles, catalog/catalog.yaml, which lists the ones packaged, and the
// signatures a release adds next to it.
//
//go:embed *.yaml catalog
var Catalog embed.FS

// Programs holds the capture programs shipped with each release: read-only
// recipes the scraper runs against a device (D73).
//
//go:embed programs
var Programs embed.FS

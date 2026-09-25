// Package profiles publishes the profile JSON Schema and the official
// profile catalog, both embedded in the binary.
package profiles

import "embed"

// Schema holds the published JSON Schema of profile.yaml.
//
//go:embed schema/profile.schema.json
var Schema []byte

// Catalog holds the official profiles shipped with each release.
//
//go:embed *.yaml
var Catalog embed.FS

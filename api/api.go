// Package api exposes the canonical OpenAPI document.
package api

import "embed"

// FS holds api/openapi.yaml.
//
//go:embed openapi.yaml
var FS embed.FS

// Spec returns the raw OpenAPI YAML bytes.
func Spec() ([]byte, error) {
	return FS.ReadFile("openapi.yaml")
}

// Package docs embeds documentation served by the application.
package docs

import _ "embed"

// OpenAPI is the specification of the read API (openapi.yaml).
//
//go:embed openapi.yaml
var OpenAPI []byte

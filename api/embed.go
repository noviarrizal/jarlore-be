// Package apispec embeds the OpenAPI contract so the API can serve it at /openapi.yaml
// and the contract tests can validate responses against it.
package apispec

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

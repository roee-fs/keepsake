// Package frontend embeds the admin API contract the console's client is generated from.
package frontend

import _ "embed"

//go:embed openapi.json
var OpenAPI []byte

// Package api holds the ReelPin public contract and embeds it so tests and
// generators read the exact bytes that ship.
package api

import _ "embed"

// Spec is api/openapi.yaml verbatim.
//
// The contract is the artifact clients are generated from, so the bytes matter:
// the release workflow publishes this file and records its SHA-256, and CI
// fails if the published digest and this file ever differ.
//
//go:embed openapi.yaml
var Spec []byte

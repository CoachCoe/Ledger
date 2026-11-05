// Package schemas embeds Ledger's JSON Schemas so the binary validates
// records without reading files at run time.
package schemas

import "embed"

// FS holds the versioned schema tree, rooted at "v1/".
//
//go:embed v1
var FS embed.FS

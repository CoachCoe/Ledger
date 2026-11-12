// Package schemas embeds Ledger's JSON Schemas so the binary validates
// records without reading files at run time.
package schemas

import "embed"

// FS holds the versioned schema trees, rooted at "v1/" and "v2/".
//
//go:embed v1 v2
var FS embed.FS

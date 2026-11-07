// Package providers holds the gitopsify provider index (index.yaml), which
// "gitopsify provider search" and "gitopsify provider install <name>" read.
// The copy embedded here is used when the published index can't be fetched.
package providers

import _ "embed"

// Index is index.yaml as built into the binary.
//
//go:embed index.yaml
var Index []byte

// Package gm wraps the official Goodmem Go SDK (fury.io/pairsys/goodmem) with a
// small constructor. The sync engine uses the returned *goodmem.Client's
// services directly (Spaces, Memories, Embedders, …).
package gm

import "fury.io/pairsys/goodmem"

// New returns a Goodmem SDK client for baseURL (without the /v1 suffix) and
// apiKey.
func New(baseURL, apiKey string) (*goodmem.Client, error) {
	return goodmem.New(baseURL, apiKey)
}

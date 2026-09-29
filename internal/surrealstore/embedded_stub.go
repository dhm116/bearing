//go:build !surrealembed

package surrealstore

import "context"

// EmbeddedAvailable reports whether this binary can run SurrealDB in-process.
const EmbeddedAvailable = false

// OpenEmbedded always fails in builds without the surrealembed tag.
func OpenEmbedded(context.Context, string, string, string) (*Store, error) {
	return nil, ErrEmbeddedUnavailable
}

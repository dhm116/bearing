package duckstore

import (
	"context"
	"fmt"
	"net/url"
	"path"

	"bearing.example/pkg/store"
)

// usage is part of every refused URL's error. It never repeats the URL.
const usage = "use mem:// or duckdb:///absolute/path.duckdb"

func init() {
	store.Register("mem", openURL)
	store.Register("duckdb", openURL)
}

// openURL opens the database a mem:// or duckdb:///absolute/path URL names.
func openURL(ctx context.Context, u *url.URL) (store.Store, error) {
	var file string
	switch {
	case u.Scheme == "mem" && u.Opaque == "" && u.Host == "" && u.Path == "":
	case u.Scheme == "duckdb" && u.Opaque == "" && u.Host == "" && path.IsAbs(u.Path):
		file = u.Path
	default:
		return nil, fmt.Errorf("unsupported URL with scheme %q; %s", u.Scheme, usage)
	}
	return Open(ctx, Config{Path: file})
}

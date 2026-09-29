//go:build surrealembed

package surrealstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	surrealdb "github.com/surrealdb/surrealdb.c.go"
)

// EmbeddedAvailable reports whether this binary can run SurrealDB in-process.
const EmbeddedAvailable = true

// OpenEmbedded runs SurrealDB inside this process. endpoint is "mem://" for
// a throwaway database or "surrealkv://path/to/dir" for one kept on disk.
func OpenEmbedded(ctx context.Context, endpoint, namespace, database string) (*Store, error) {
	db, err := surrealdb.Open(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("surrealstore: open embedded %s: %w", endpoint, err)
	}
	q := &embeddedQuerier{db: db}
	if err := useDatabase(ctx, q, namespace, database, db.Use); err != nil {
		db.Close()
		return nil, err
	}
	return New(ctx, q)
}

type embeddedQuerier struct{ db surrealdb.Driver }

func (e *embeddedQuerier) Query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	sql, vars, skip, err := inlineNested(sql, vars)
	if err != nil {
		return nil, err
	}
	res, err := e.db.Query(ctx, sql, vars)
	if err != nil {
		return nil, err
	}
	res = res[min(skip, len(res)):]
	out := make([]any, len(res))
	var msgs []string
	for i, r := range res {
		if r.Err != nil {
			msgs = append(msgs, r.Err.Error())
			continue
		}
		out[i] = r.Value
	}
	if len(msgs) > 0 {
		return nil, statementError(msgs)
	}
	return out, nil
}

func (e *embeddedQuerier) Close(context.Context) error {
	e.db.Close()
	return nil
}

// inlineNested works around the embedded driver, which can't pass arrays of
// objects (such as a fact's sources) as query variables. Those variables are
// written into the query as JSON literals, which SurrealQL parses as objects
// and arrays, in LET statements ahead of it. skip is how many results those
// statements add.
func inlineNested(sql string, vars map[string]any) (string, map[string]any, int, error) {
	var names []string
	for k, v := range vars {
		if hasObjectArray(reflect.ValueOf(v)) {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return sql, vars, 0, nil
	}
	slices.Sort(names)
	rest := make(map[string]any, len(vars))
	for k, v := range vars {
		rest[k] = v
	}
	var b strings.Builder
	for _, k := range names {
		lit, err := json.Marshal(vars[k])
		if err != nil {
			return "", nil, 0, fmt.Errorf("encode variable %s: %w", k, err)
		}
		fmt.Fprintf(&b, "LET $%s = %s;\n", k, lit)
		delete(rest, k)
	}
	return b.String() + sql, rest, len(names), nil
}

func hasObjectArray(v reflect.Value) bool {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			e := v.Index(i)
			for e.Kind() == reflect.Interface || e.Kind() == reflect.Pointer {
				if e.IsNil() {
					break
				}
				e = e.Elem()
			}
			switch e.Kind() {
			case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array:
				if e.Kind() == reflect.Slice && e.Type().Elem().Kind() == reflect.Uint8 {
					continue
				}
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if hasObjectArray(it.Value()) {
				return true
			}
		}
	}
	return false
}

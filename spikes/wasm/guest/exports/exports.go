// Package exports implements the bodies of an adapter module's Extism exports
// (describe, sync, handle) for any adapter.Adapter. Each module's main package
// declares the //go:wasmexport wrappers and calls these.
//
// Exports take and return the same JSON as the stdio protocol's params and
// results. Errors are reported with pdk.SetError as a JSON adapter.Error.
package exports

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/extism/go-pdk"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

func fail(err error) int32 {
	var e *adapter.Error
	if !errors.As(err, &e) {
		e = &adapter.Error{Code: adapter.CodeInternal, Message: err.Error()}
	}
	b, _ := json.Marshal(e)
	pdk.SetErrorString(string(b))
	return 1
}

func output(v any) int32 {
	b, err := json.Marshal(v)
	if err != nil {
		return fail(err)
	}
	pdk.Output(b)
	return 0
}

// Describe answers the describe export.
func Describe(a adapter.Adapter) int32 {
	res, err := a.Describe(context.Background())
	if err != nil {
		return fail(err)
	}
	return output(res)
}

// Sync answers the sync export: one page per call.
func Sync(a adapter.Adapter) int32 {
	var p adapter.SyncParams
	if err := json.Unmarshal(pdk.Input(), &p); err != nil {
		return fail(&adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()})
	}
	res, err := a.Sync(context.Background(), p)
	if err != nil {
		return fail(err)
	}
	if res.Observations == nil {
		res.Observations = []model.Observation{}
	}
	return output(res)
}

// Handle answers the handle export.
func Handle(a adapter.Adapter) int32 {
	var p adapter.HandleParams
	if err := json.Unmarshal(pdk.Input(), &p); err != nil {
		return fail(&adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()})
	}
	res, err := a.Handle(context.Background(), p)
	if err != nil {
		return fail(err)
	}
	return output(res)
}

package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"bearing.example/pkg/model"
)

// maxMessage bounds a single protocol line. Sync pages should stay well
// below it; adapters that hit it should return smaller pages.
const maxMessage = 32 << 20

// Serve answers protocol requests read from r and writes responses to w
// until r is closed or ctx is cancelled. Requests are handled one at a time.
func Serve(ctx context.Context, a Adapter, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &Error{Code: CodeParseError, Message: err.Error()}}); err != nil {
				return err
			}
			continue
		}
		if req.ID == nil {
			// Notifications get no response; the protocol defines none yet.
			continue
		}
		result, rpcErr := dispatch(ctx, a, req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ServeStdio is Serve on the process's stdin and stdout, for use in main.
func ServeStdio(a Adapter) error {
	return Serve(context.Background(), a, os.Stdin, os.Stdout)
}

func dispatch(ctx context.Context, a Adapter, req request) (any, *Error) {
	switch req.Method {
	case MethodDescribe:
		res, err := a.Describe(ctx)
		if err != nil {
			return nil, toError(err)
		}
		if res.ProtocolVersion == "" {
			res.ProtocolVersion = ProtocolVersion
		}
		return res, nil
	case MethodSync:
		var p SyncParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		res, err := a.Sync(ctx, p)
		if err != nil {
			return nil, toError(err)
		}
		if res.Observations == nil {
			res.Observations = []model.Observation{}
		}
		return res, nil
	case MethodHandle:
		var p HandleParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		res, err := a.Handle(ctx, p)
		if err != nil {
			return nil, toError(err)
		}
		if res.Observations == nil {
			res.Observations = []model.Observation{}
		}
		return res, nil
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("unknown method %q", req.Method)}
	}
}

func decodeParams(raw json.RawMessage, v any) *Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	return nil
}

func toError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

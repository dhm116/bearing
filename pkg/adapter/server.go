package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// maxMessage bounds a single protocol line. Sync pages should stay well
// below it; adapters that hit it should return smaller pages.
const maxMessage = 32 << 20

// Serve answers protocol requests read from r and writes responses to w
// until r is closed or ctx is cancelled. Requests are handled one at a time.
//
// Each request runs in a server span that continues the caller's trace when
// the request carries trace context in _meta.
func Serve(ctx context.Context, a Adapter, r io.Reader, w io.Writer) error {
	log := telemetry.Logger(pkgName)
	name := "unknown"
	if d, err := a.Describe(ctx); err == nil && d.Name != "" {
		name = d.Name
	}
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
			log.WarnContext(ctx, "adapter received an unparseable request", "error", err.Error(), string(attrAdapter), name)
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
		resp := serveOne(ctx, a, name, req)
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return sc.Err()
}

func serveOne(ctx context.Context, a Adapter, name string, req request) response {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(req.Meta))
	ctx, span := tracer.Start(ctx, req.Method,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.RPCSystemNameJSONRPC,
			semconv.RPCMethod(req.Method),
			attribute.String("jsonrpc.request.id", string(req.ID)),
			attrAdapter.String(name),
		))
	defer span.End()
	start := time.Now()

	result, rpcErr := dispatch(ctx, a, req)

	attrs := []attribute.KeyValue{semconv.RPCMethod(req.Method), attrAdapter.String(name)}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		code := strconv.Itoa(rpcErr.Code)
		attrs = append(attrs, semconv.ErrorTypeKey.String(code))
		span.SetAttributes(semconv.RPCResponseStatusCode(code))
		if rpcErr.Code == CodeNotSupported {
			// An optional method the adapter skips is not a failure.
			span.SetStatus(codes.Unset, "")
		} else {
			telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "adapter request failed", rpcErr,
				semconv.RPCMethod(req.Method), attrAdapter.String(name))
		}
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	rpcServerDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	return resp
}

// ServeStdio is Serve on the process's stdin and stdout, for use in main.
func ServeStdio(ctx context.Context, a Adapter) error {
	return Serve(ctx, a, os.Stdin, os.Stdout)
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
		countEmitted(ctx, req.Method, res.Observations)
		trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(res.Observations)), attribute.Bool("bearing.sync.done", res.Done))
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
		countEmitted(ctx, req.Method, res.Observations)
		trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(res.Observations)))
		return res, nil
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("unknown method %q", req.Method)}
	}
}

func countEmitted(ctx context.Context, method string, obs []model.Observation) {
	byKind := map[model.Kind]int64{}
	for _, o := range obs {
		byKind[o.Data.Entity.Kind]++
	}
	for k, n := range byKind {
		observationsEmitted.Add(ctx, n, metric.WithAttributes(semconv.RPCMethod(method), attrKind.String(string(k))))
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

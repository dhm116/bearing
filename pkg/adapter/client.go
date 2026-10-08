package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Client talks to one adapter over the protocol. It is safe for concurrent
// use; calls are serialized because the protocol handles one request at a time.
type Client struct {
	name   string
	mu     sync.Mutex
	enc    *json.Encoder
	sc     *bufio.Scanner
	nextID int
	close  func() error
}

// NewClient speaks the protocol over an existing reader and writer, such as
// in-memory pipes in tests. name labels the adapter in telemetry.
func NewClient(name string, r io.Reader, w io.Writer) *Client {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	return &Client{name: name, enc: json.NewEncoder(w), sc: sc, close: func() error { return nil }}
}

// Start launches an adapter executable and connects to its stdin and stdout.
// The adapter's stderr, where its console telemetry goes, is passed through
// to this process's stderr. The adapter inherits this process's environment,
// including OTEL_* settings.
func Start(ctx context.Context, name string, args ...string) (*Client, error) {
	// Running the adapter the caller names is this function's job.
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the adapter command is the caller's choice
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start adapter %s: %w", name, err)
	}
	telemetry.Logger(pkgName).InfoContext(ctx, "adapter started",
		string(attrAdapter), filepath.Base(name), "pid", cmd.Process.Pid)
	c := NewClient(filepath.Base(name), stdout, stdin)
	c.close = func() error {
		// Closing stdin tells the adapter to exit; Wait reports how it did.
		closeErr := stdin.Close()
		return errors.Join(cmd.Wait(), closeErr)
	}
	return c, nil
}

// Name is the adapter's label in telemetry.
func (c *Client) Name() string { return c.name }

// Close ends the session and, for started adapters, waits for the process.
func (c *Client) Close() error { return c.close() }

func (c *Client) call(ctx context.Context, method string, params, result any) (err error) {
	ctx, span := tracer.Start(ctx, method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.RPCSystemNameJSONRPC, semconv.RPCMethod(method), attrAdapter.String(c.name)))
	start := time.Now()
	defer func() {
		attrs := []attribute.KeyValue{semconv.RPCMethod(method), attrAdapter.String(c.name)}
		var rpcErr *Error
		switch {
		case err == nil:
		case errors.As(err, &rpcErr) && rpcErr.Code == CodeNotSupported:
			attrs = append(attrs, semconv.ErrorTypeKey.String(rpcErr.ErrorType()))
			span.SetAttributes(semconv.RPCResponseStatusCode(rpcErr.ErrorType()))
		default:
			attrs = append(attrs, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		rpcClientDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}()

	meta := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(meta))

	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := json.RawMessage(strconv.Itoa(c.nextID))
	span.SetAttributes(attribute.String("jsonrpc.request.id", string(id)))
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	if err := c.enc.Encode(request{JSONRPC: "2.0", ID: id, Method: method, Params: raw, Meta: meta}); err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return fmt.Errorf("%s: read: %w", method, err)
		}
		return fmt.Errorf("%s: adapter closed the connection", method)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if string(resp.ID) != string(id) {
		return fmt.Errorf("%s: response id %s does not match request id %s", method, resp.ID, id)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return json.Unmarshal(resp.Result, result)
}

// Describe calls bearing.describe.
func (c *Client) Describe(ctx context.Context) (DescribeResult, error) {
	var res DescribeResult
	err := c.call(ctx, MethodDescribe, nil, &res)
	return res, err
}

// Sync calls bearing.sync for one page.
func (c *Client) Sync(ctx context.Context, p SyncParams) (SyncResult, error) {
	var res SyncResult
	err := c.call(ctx, MethodSync, p, &res)
	return res, err
}

// Handle calls bearing.handle. It validates every observation and truncates
// its times to microseconds. Unlike SyncAll it fails on the first invalid
// observation: a delivery is one event, not a sweep to carry on with.
func (c *Client) Handle(ctx context.Context, p HandleParams) (HandleResult, error) {
	var res HandleResult
	if err := c.call(ctx, MethodHandle, p, &res); err != nil {
		return res, err
	}
	for i, o := range res.Observations {
		if err := checkObservation(ctx, c.name, o); err != nil {
			return HandleResult{}, fmt.Errorf("%s: observation %d: %w", MethodHandle, i, err)
		}
	}
	return res, nil
}

// checkObservation validates an adapter's observation on the host
// (model.ValidateAdapterObservation), counting failures, and only then
// truncates its times to microseconds, so an invalid timestamp is rejected
// rather than repaired.
func checkObservation(ctx context.Context, adapterName string, o *eventv1alpha1.Observation) error {
	if err := model.ValidateAdapterObservation(o); err != nil {
		observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(adapterName)))
		return err
	}
	model.TruncateTimes(o)
	return nil
}

// ErrTooManyPages is returned by SyncAll when an adapter never reports done.
var ErrTooManyPages = errors.New("adapter returned too many pages without finishing")

// Syncer is anything that serves sync pages: a Client, or an Adapter called
// in-process.
type Syncer interface {
	Sync(context.Context, SyncParams) (SyncResult, error)
}

// SyncSummary counts what SyncAll did with an adapter's observations.
type SyncSummary struct {
	// Pages is how many sync pages the adapter returned.
	Pages int
	// Accepted is how many observations SyncAll passed to emit, with their
	// rejected claims already removed.
	Accepted int
	// RejectedObservations were skipped whole: they failed validation, or
	// could not be decoded at all.
	RejectedObservations int
	// RejectedClaims were removed from observations that were accepted.
	RejectedClaims int
}

// Complete reports whether every observation the adapter sent was accepted
// (perhaps without some claims). A sync that skipped observations has not
// seen every entity it returned, so the core must not count it as complete
// when it looks for entities that went missing (docs/spec/data-model.md,
// "Sync completeness").
func (s SyncSummary) Complete() bool { return s.RejectedObservations == 0 }

// maxRejectionLogs is how many rejections SyncAll logs one by one in a sync;
// the rest are only counted, so a broken adapter can't flood the log.
const maxRejectionLogs = 20

// screenObservation validates an observation from a sync
// (model.ValidateAdapterObservation) and says whether to keep it. An
// observation with only claim-scoped problems is kept without those claims;
// any other failure rejects it whole. Either way the sync goes on, and the
// rejection is counted and, if warn is set, logged, so one bad observation
// doesn't discard a whole sync. Times are truncated to microseconds only
// after validation, so an invalid timestamp is rejected rather than
// repaired.
func screenObservation(ctx context.Context, log *slog.Logger, adapterName string, o *eventv1alpha1.Observation, warn bool) (keep bool, claims int) {
	err := model.ValidateAdapterObservation(o)
	if err == nil {
		model.TruncateTimes(o)
		return true, 0
	}
	id := model.Clip(o.GetId())
	if n := model.DropRejectedClaims(o, err); n > 0 {
		if warn {
			log.WarnContext(ctx, "adapter claims rejected", string(attrAdapter), adapterName,
				"observation", id, "claims", n, "reason", err.Error())
		}
		model.TruncateTimes(o)
		return true, n
	}
	observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(adapterName)))
	if warn {
		log.WarnContext(ctx, "adapter observation rejected", string(attrAdapter), adapterName,
			"observation", id, "reason", err.Error())
	}
	return false, 0
}

// SyncAll pages through a full sync, validating every observation
// (model.ValidateAdapterObservation), truncating its times to microseconds
// and passing it to emit. An observation that fails validation is skipped,
// or, if only some of its claims are rejected, passed on without them; the
// summary counts both and the sync goes on. An observation that can't be
// decoded (an unknown field, say) counts as rejected too. maxPages guards
// against adapters that never finish. The whole sync runs in one span, with
// each page as a child.
func SyncAll(ctx context.Context, a Syncer, config json.RawMessage, maxPages int, emit func(*eventv1alpha1.Observation) error) (sum SyncSummary, err error) {
	name := "in-process"
	if n, ok := a.(interface{ Name() string }); ok {
		name = n.Name()
	}
	log := telemetry.Logger(pkgName)
	ctx, span := tracer.Start(ctx, "bearing.adapter.sync", trace.WithAttributes(attrAdapter.String(name)))
	start := time.Now()
	defer func() {
		span.SetAttributes(attrPages.Int(sum.Pages), attrCount.Int(sum.Accepted))
		attrs := []attribute.KeyValue{attrAdapter.String(name)}
		if err != nil {
			attrs = append(attrs, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
			telemetry.Fail(ctx, span, log, "adapter sync failed", err, attrAdapter.String(name), attrPages.Int(sum.Pages))
		} else {
			log.InfoContext(ctx, "adapter sync finished", string(attrAdapter), name,
				string(attrPages), sum.Pages, string(attrCount), sum.Accepted, "duration", time.Since(start).String(),
				"rejected_observations", sum.RejectedObservations, "rejected_claims", sum.RejectedClaims)
		}
		syncDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}()

	cursor := ""
	for sum.Pages < maxPages {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		res, err := a.Sync(ctx, SyncParams{Config: config, Cursor: cursor})
		if err != nil {
			return sum, fmt.Errorf("sync page %d: %w", sum.Pages+1, err)
		}
		sum.Pages++
		syncPages.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name)))
		for _, why := range res.Undecodable {
			observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name)))
			sum.RejectedObservations++
			if sum.RejectedObservations+sum.RejectedClaims <= maxRejectionLogs {
				log.WarnContext(ctx, "adapter observation rejected", string(attrAdapter), name, "reason", "malformed: "+why)
			}
		}
		for _, o := range res.Observations {
			warn := sum.RejectedObservations+sum.RejectedClaims < maxRejectionLogs
			keep, claims := screenObservation(ctx, log, name, o, warn)
			sum.RejectedClaims += claims
			if !keep {
				sum.RejectedObservations++
				continue
			}
			observationsReceived.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name), attrKind.String(o.GetData().GetEntity().GetKind())))
			sum.Accepted++
			if err := emit(o); err != nil {
				return sum, err
			}
		}
		if res.Done {
			return sum, nil
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return sum, fmt.Errorf("sync page %d: adapter is not done but returned no new cursor", sum.Pages)
		}
		cursor = res.NextCursor
	}
	return sum, ErrTooManyPages
}

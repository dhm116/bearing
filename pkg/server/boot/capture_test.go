package boot

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// What the process emitted through OpenTelemetry, for tests that look for
// what must not be there. They install themselves once, in installCapture, and
// the tests that read them look for a string only they use, so other tests
// running beside them don't disturb the result.
var (
	captureOnce sync.Once
	spanLog     = tracetest.NewSpanRecorder()
	metricRead  = sdkmetric.NewManualReader()
	logs        = &logCapture{}
)

// logCapture keeps every log record as text.
type logCapture struct {
	mu   sync.Mutex
	text []string
}

func (c *logCapture) Export(_ context.Context, recs []sdklog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range recs {
		var b strings.Builder
		b.WriteString(recs[i].Body().String())
		recs[i].WalkAttributes(func(kv attribute.KeyValue) bool {
			fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
			return true
		})
		c.text = append(c.text, b.String())
	}
	return nil
}

func (c *logCapture) Shutdown(context.Context) error   { return nil }
func (c *logCapture) ForceFlush(context.Context) error { return nil }

func (c *logCapture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.text, "\n")
}

func installCapture() {
	captureOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanLog)))
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricRead)))
		logglobal.SetLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs))))
	})
}

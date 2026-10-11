package boot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// spanText renders every span that has ended, with its events and attributes.
func spanText() string {
	var b strings.Builder
	for _, s := range spanLog.Ended() {
		fmt.Fprintf(&b, "%s %v %s\n", s.Name(), s.Attributes(), s.Status().Description)
		for _, e := range s.Events() {
			fmt.Fprintf(&b, "  %s %v\n", e.Name, e.Attributes)
		}
	}
	return b.String()
}

// metricText renders every data point collected, with its attributes.
func metricText(t *testing.T) string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricRead.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(rm)
}

// checkpointFailures sums the writer's failed runs at stage.
func checkpointFailures(t *testing.T, stage string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricRead.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "bearing.audit.checkpoints" {
				continue
			}
			for _, dp := range sum.DataPoints {
				res, _ := dp.Attributes.Value(attribute.Key("bearing.result"))
				st, _ := dp.Attributes.Value(attribute.Key("bearing.stage"))
				if res.AsString() == "failed" && st.AsString() == stage {
					n += dp.Value
				}
			}
		}
	}
	return n
}

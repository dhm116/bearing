package authentik_test

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// logs keeps every log record the adapter wrote as text, for tests that
// look for what must not be there.
var (
	captureOnce sync.Once
	logs        = &logCapture{}
)

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
		logglobal.SetLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs))))
	})
}

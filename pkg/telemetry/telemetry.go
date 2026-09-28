// Package telemetry sets up OpenTelemetry logs, traces and metrics for every
// Bearing binary, and gives packages a single place to get their tracer,
// meter and logger.
//
// Configuration follows the standard OpenTelemetry environment variables:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT     send everything over OTLP/HTTP to this endpoint
//	OTEL_TRACES_EXPORTER            otlp | console | none
//	OTEL_METRICS_EXPORTER           otlp | console | none
//	OTEL_LOGS_EXPORTER              otlp | console | none
//	OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES
//	BEARING_LOG_LEVEL               debug | info | warn | error (default info)
//
// When no endpoint is set, logs go to stderr and traces and metrics are off.
// Console exporters always write to stderr, never stdout, because adapters use
// stdout for the adapter protocol.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope prefix for Bearing's own telemetry.
const ScopeName = "bearing.example"

// Config describes the binary being set up.
type Config struct {
	ServiceName    string
	ServiceVersion string
	// Stderr is where console exporters write. Defaults to os.Stderr.
	Stderr io.Writer
	// Getenv reads configuration. Defaults to os.Getenv.
	Getenv func(string) string
}

// Exporter names accepted in OTEL_*_EXPORTER.
const (
	ExporterOTLP    = "otlp"
	ExporterConsole = "console"
	ExporterNone    = "none"
)

// Setup installs global OpenTelemetry providers, points slog's default
// logger at the OpenTelemetry log pipeline, and routes OpenTelemetry's own
// errors to that logger. Call the returned shutdown before the process exits
// so buffered telemetry is flushed.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if name := cfg.Getenv("OTEL_SERVICE_NAME"); name != "" {
		cfg.ServiceName = name
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		// Schema URL conflicts between the SDK default and ours are not
		// fatal; fall back to our attributes alone.
		res = resource.NewSchemaless(semconv.ServiceName(cfg.ServiceName), semconv.ServiceVersion(cfg.ServiceVersion))
	}

	otlp := cfg.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	pick := func(signal, fallback string) string {
		if v := strings.ToLower(strings.TrimSpace(cfg.Getenv("OTEL_" + signal + "_EXPORTER"))); v != "" {
			return v
		}
		if otlp {
			return ExporterOTLP
		}
		return fallback
	}

	var shutdowns []func(context.Context) error
	shutdown = func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (func(context.Context) error, error) {
		return shutdown, errors.Join(err, shutdown(ctx))
	}

	// Traces.
	tracerOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	switch ex := pick("TRACES", ExporterNone); ex {
	case ExporterOTLP:
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("trace exporter: %w", err))
		}
		tracerOpts = append(tracerOpts, sdktrace.WithBatcher(exp))
	case ExporterConsole:
		exp, err := stdouttrace.New(stdouttrace.WithWriter(cfg.Stderr))
		if err != nil {
			return fail(fmt.Errorf("trace exporter: %w", err))
		}
		tracerOpts = append(tracerOpts, sdktrace.WithSyncer(exp))
	case ExporterNone:
	default:
		return fail(fmt.Errorf("OTEL_TRACES_EXPORTER: unsupported value %q", ex))
	}
	tp := sdktrace.NewTracerProvider(tracerOpts...)
	shutdowns = append(shutdowns, tp.Shutdown)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// Metrics.
	meterOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	switch ex := pick("METRICS", ExporterNone); ex {
	case ExporterOTLP:
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("metric exporter: %w", err))
		}
		meterOpts = append(meterOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	case ExporterConsole:
		exp, err := stdoutmetric.New(stdoutmetric.WithWriter(cfg.Stderr))
		if err != nil {
			return fail(fmt.Errorf("metric exporter: %w", err))
		}
		meterOpts = append(meterOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(30*time.Second))))
	case ExporterNone:
	default:
		return fail(fmt.Errorf("OTEL_METRICS_EXPORTER: unsupported value %q", ex))
	}
	mp := sdkmetric.NewMeterProvider(meterOpts...)
	shutdowns = append(shutdowns, mp.Shutdown)
	otel.SetMeterProvider(mp)

	// Logs.
	logOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	switch ex := pick("LOGS", ExporterConsole); ex {
	case ExporterOTLP:
		exp, err := otlploghttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("log exporter: %w", err))
		}
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	case ExporterConsole:
		exp, err := stdoutlog.New(stdoutlog.WithWriter(cfg.Stderr))
		if err != nil {
			return fail(fmt.Errorf("log exporter: %w", err))
		}
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	case ExporterNone:
	default:
		return fail(fmt.Errorf("OTEL_LOGS_EXPORTER: unsupported value %q", ex))
	}
	lp := sdklog.NewLoggerProvider(logOpts...)
	shutdowns = append(shutdowns, lp.Shutdown)
	global.SetLoggerProvider(lp)

	if lvl := cfg.Getenv("BEARING_LOG_LEVEL"); lvl != "" {
		if err := level.UnmarshalText([]byte(lvl)); err != nil {
			return fail(fmt.Errorf("BEARING_LOG_LEVEL: %w", err))
		}
	}
	logger := slog.New(leveled{otelslog.NewHandler(ScopeName, otelslog.WithLoggerProvider(lp), otelslog.WithVersion(cfg.ServiceVersion))})
	slog.SetDefault(logger)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("opentelemetry error", "error", err)
	}))
	return shutdown, nil
}

// Tracer returns the tracer for a Bearing package, e.g. Tracer("pkg/adapter").
func Tracer(pkg string) trace.Tracer {
	return otel.Tracer(ScopeName + "/" + pkg)
}

// Meter returns the meter for a Bearing package.
func Meter(pkg string) metric.Meter {
	return otel.Meter(ScopeName + "/" + pkg)
}

// Logger returns a slog logger for a Bearing package, backed by the global
// OpenTelemetry logger provider. It is safe to call before Setup: the global
// provider delegates to whatever Setup installs later. Use the *Context
// methods so records carry the current trace and span IDs.
func Logger(pkg string) *slog.Logger {
	return slog.New(leveled{otelslog.NewHandler(ScopeName + "/" + pkg)})
}

// level is the minimum level for Bearing's loggers, set by Setup.
var level slog.LevelVar

// leveled drops records below level before they reach OpenTelemetry.
type leveled struct{ slog.Handler }

func (h leveled) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= level.Level() && h.Handler.Enabled(ctx, l)
}

func (h leveled) WithAttrs(attrs []slog.Attr) slog.Handler {
	return leveled{h.Handler.WithAttrs(attrs)}
}

func (h leveled) WithGroup(name string) slog.Handler {
	return leveled{h.Handler.WithGroup(name)}
}

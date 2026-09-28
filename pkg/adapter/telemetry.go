package adapter

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/adapter"

var (
	tracer = telemetry.Tracer(pkgName)
	meter  = telemetry.Meter(pkgName)
)

// Attribute keys shared by adapter spans and metrics.
const (
	attrAdapter = attribute.Key("bearing.adapter.name")
	attrKind    = attribute.Key("bearing.entity.kind")
	attrPages   = attribute.Key("bearing.sync.pages")
	attrCount   = attribute.Key("bearing.observations.count")
)

// Metric instruments. See docs/telemetry.md for the catalog.
var (
	rpcServerDuration = mustHistogram("bearing.adapter.rpc.server.duration",
		"Time an adapter spent answering one protocol request.", "s")
	rpcClientDuration = mustHistogram("bearing.adapter.rpc.client.duration",
		"Time the core waited for one adapter protocol response.", "s")
	syncDuration = mustHistogram("bearing.adapter.sync.duration",
		"Duration of a full adapter sync, first page to done.", "s")
	syncPages = mustCounter("bearing.adapter.sync.pages",
		"Sync pages received from adapters.", "{page}")
	observationsEmitted = mustCounter("bearing.adapter.observations.emitted",
		"Observations an adapter returned, by method and entity kind.", "{observation}")
	observationsReceived = mustCounter("bearing.adapter.observations.received",
		"Valid observations the core accepted from adapters, by entity kind.", "{observation}")
	observationsInvalid = mustCounter("bearing.adapter.observations.invalid",
		"Observations the core rejected because they failed schema validation.", "{observation}")
)

func mustHistogram(name, desc, unit string) metric.Float64Histogram {
	h, err := meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		panic(err)
	}
	return h
}

func mustCounter(name, desc, unit string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		panic(err)
	}
	return c
}

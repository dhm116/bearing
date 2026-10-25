package kubernetes

import (
	"go.opentelemetry.io/otel/attribute"

	"bearing.example/pkg/telemetry"
)

const pkgName = "adapters/kubernetes"

var tracer = telemetry.Tracer(pkgName)

const (
	attrPhase   = attribute.Key("bearing.kubernetes.sync.phase")
	attrCluster = attribute.Key("bearing.kubernetes.cluster")
)

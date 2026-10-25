package authentik

import (
	"go.opentelemetry.io/otel/attribute"

	"bearing.example/pkg/telemetry"
)

const pkgName = "adapters/authentik"

var tracer = telemetry.Tracer(pkgName)

const attrPhase = attribute.Key("bearing.authentik.sync.phase")

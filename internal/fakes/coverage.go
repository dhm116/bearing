package fakes

import (
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/vocab"
)

// The coverage trail's three tools (ADR 13 spike, item 5): a Datadog
// monitor on payments-api that pages a PagerDuty service, and the Terraform
// that configures the monitor. No server: the observations are built
// directly, as their adapters would emit them.
const (
	DatadogNamespace   = "com.datadoghq"
	TerraformNamespace = "com.hashicorp.terraform"
	PagerDutyNamespace = "com.pagerduty"

	DatadogSource   = "datadog-acme"
	TerraformSource = "terraform-acme"
	PagerDutySource = "pagerduty-acme"

	// MonitorKey is the monitor that watches payments-api.
	MonitorKey = "datadog:monitor/123"
	// MonitorResourceKey is the Terraform resource that configures it.
	MonitorResourceKey = "terraform:resource/datadog_monitor.payments_latency"
	// CheckoutKey is the PagerDuty service the monitor pages.
	CheckoutKey = "pagerduty:service/checkout"

	// CheckoutHandle is the monitor's notification handle for checkout;
	// StaleHandle names a service PagerDuty no longer has.
	CheckoutHandle = "@pagerduty-checkout"
	StaleHandle    = "@pagerduty-checkout-old"
)

// CoverageModules are the vocabulary modules the three adapters would
// publish: their kinds, and the relations proposing rules make between
// them. The relations are not core: watches and pages are Datadog's
// words, describes Terraform's, and covered_by (the joined link) is
// PagerDuty's.
func CoverageModules() []vocab.Module {
	rel := func(name string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsRelation, Cardinality: "many", Conflict: "none"}
	}
	attr := func(name, typ string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsAttribute, Type: typ, Cardinality: "one", Conflict: "none"}
	}
	return []vocab.Module{
		{Owner: vocab.OwnerAdapter, Namespace: DatadogNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Monitor", Is: vocab.IsKind}, rel("watches"), rel("pages"), attr("monitor_tags", "json"), attr("message", "string"),
		}},
		{Owner: vocab.OwnerAdapter, Namespace: TerraformNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Resource", Is: vocab.IsKind}, rel("describes"), attr("monitor_id", "string"), attr("module_source", "string"),
		}},
		{Owner: vocab.OwnerAdapter, Namespace: PagerDutyNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Service", Is: vocab.IsKind}, rel("covered_by"), attr("service_name", "string"),
		}},
	}
}

// DatadogMonitor is monitor 123 as Datadog's API reports it, notifying
// handle. Attribute names avoid the core's tags and name, whose domains
// don't cover an unmapped kind.
func DatadogMonitor(at time.Time, handle string) *eventv1alpha1.Observation {
	return entity(DatadogSource, at, vocab.Full(DatadogNamespace, "Monitor"), MonitorKey, map[string]any{
		"monitor_tags": []any{"service:payments-api", "env:prod"},
		"message":      "High latency " + handle,
	})
}

// TerraformMonitor is the Terraform resource that configures monitor 123:
// the same tags and message (the configured intent of the same claim), and
// the payments-api repository its module comes from.
func TerraformMonitor(at time.Time, handle string) *eventv1alpha1.Observation {
	return entity(TerraformSource, at, vocab.Full(TerraformNamespace, "Resource"), MonitorResourceKey, map[string]any{
		"monitor_id":    "123",
		"monitor_tags":  []any{"service:payments-api", "env:prod"},
		"message":       "High latency " + handle,
		"module_source": "github.com/acme/payments-api//terraform/monitors?ref=v1.4.2",
	})
}

// PagerDutyService is the checkout service.
func PagerDutyService(at time.Time) *eventv1alpha1.Observation {
	return entity(PagerDutySource, at, vocab.Full(PagerDutyNamespace, "Service"), CheckoutKey, map[string]any{"service_name": "checkout"})
}

func entity(source string, at time.Time, kind, key string, attrs map[string]any) *eventv1alpha1.Observation {
	st, err := structpb.NewStruct(attrs)
	if err != nil {
		panic(err) // fixed literals above
	}
	return model.NewObservation(source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: kind, Key: key, Attributes: st.GetFields()},
	})
}

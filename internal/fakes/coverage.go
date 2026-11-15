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
	// OperatorNamespace is Acme's own module, which holds covered_by.
	OperatorNamespace = "com.acme"

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
// publish: each adapter's kinds, relations and attributes, all in its own
// namespace. The joined covered_by link is not here: the server's joiner
// makes it, so it is an operator term (see CoverageOperator).
func CoverageModules() []vocab.Module {
	rel := func(name string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsRelation, Cardinality: "many", Conflict: "none"}
	}
	attr := func(name, typ string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsAttribute, Type: typ, Cardinality: "one", Conflict: "none"}
	}
	return []vocab.Module{
		{Owner: vocab.OwnerAdapter, Namespace: DatadogNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Monitor", Is: vocab.IsKind}, rel("watches"), rel("pages"), attr("tags", "json"), attr("message", "string"),
		}},
		{Owner: vocab.OwnerAdapter, Namespace: TerraformNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Resource", Is: vocab.IsKind}, rel("describes"), attr("monitor_id", "string"), attr("tags", "json"), attr("message", "string"), attr("module_source", "string"),
		}},
		{Owner: vocab.OwnerAdapter, Namespace: PagerDutyNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
			{Name: "Service", Is: vocab.IsKind}, attr("name", "string"),
		}},
	}
}

// CoverageOperator is the operator module that holds the joined link, as
// testdata/vocab/acme.yaml holds Acme's other terms: the joiner is the
// operator's, so its facts live in the operator's namespace, never an
// adapter's.
func CoverageOperator() vocab.Module {
	return vocab.Module{Owner: vocab.OwnerOperator, Namespace: OperatorNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "sha256"}, Terms: []vocab.Term{
		{Name: "covered_by", Is: vocab.IsRelation, Cardinality: "many", Conflict: "none"},
	}}
}

// DatadogMonitor is monitor 123 as Datadog's API reports it, notifying
// handle. Its Kubernetes tags name the workload; attribute names are
// Datadog's own (com.datadoghq:tags), not the core's.
func DatadogMonitor(at time.Time, handle string) *eventv1alpha1.Observation {
	return entity(DatadogSource, at, vocab.Full(DatadogNamespace, "Monitor"), MonitorKey, map[string]any{
		vocab.Full(DatadogNamespace, "tags"):    monitorTags(),
		vocab.Full(DatadogNamespace, "message"): "High latency " + handle,
	})
}

// TerraformMonitor is the Terraform resource that configures monitor 123:
// the same tags and message (the configured intent of the same claim), and
// the payments-api repository its module comes from.
func TerraformMonitor(at time.Time, handle string) *eventv1alpha1.Observation {
	return entity(TerraformSource, at, vocab.Full(TerraformNamespace, "Resource"), MonitorResourceKey, map[string]any{
		vocab.Full(TerraformNamespace, "monitor_id"):    "123",
		vocab.Full(TerraformNamespace, "tags"):          monitorTags(),
		vocab.Full(TerraformNamespace, "message"):       "High latency " + handle,
		vocab.Full(TerraformNamespace, "module_source"): "github.com/acme/payments-api//terraform/monitors?ref=v1.4.2",
	})
}

// FuzzyHandle names checkout by its display name, not its key.
const FuzzyHandle = "@pagerduty-checkout-platform"

// PagerDutyService is the checkout service; its display name differs from
// its key's last segment.
func PagerDutyService(at time.Time) *eventv1alpha1.Observation {
	return entity(PagerDutySource, at, vocab.Full(PagerDutyNamespace, "Service"), CheckoutKey, map[string]any{vocab.Full(PagerDutyNamespace, "name"): "Checkout Platform"})
}

func monitorTags() []any {
	return []any{"service:payments-api", "env:prod", "kube_cluster_name:" + ClusterID, "kube_namespace:payments", "kube_deployment:payments-api"}
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

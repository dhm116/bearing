package instrument_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"bearing.example/pkg/auth"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
	"bearing.example/pkg/contracts/instrument"
)

func TestWrappedAuthorizerConforms(t *testing.T) {
	conformance.Authorizer(t, func(_ *testing.T, grants []contracts.Grant) (contracts.Authorizer, error) {
		a, err := auth.NewRoleAuthorizer(grants)
		if err != nil {
			return nil, err
		}
		return instrument.Authorizer(a, "roles"), nil
	})
}

type failingAuthorizer struct{}

func (failingAuthorizer) Authorize(context.Context, contracts.Request) (contracts.AuthDecision, error) {
	return contracts.AuthDecision{}, errors.New("relationship store is down")
}

// authzCounts returns the bearing.authz.decisions counter by "method/result".
func authzCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "bearing.authz.decisions" {
				continue
			}
			for _, dp := range sum.DataPoints {
				method, _ := dp.Attributes.Value(attribute.Key("bearing.authz.method"))
				result, _ := dp.Attributes.Value(attribute.Key("bearing.result"))
				out[method.AsString()+"/"+result.AsString()] += dp.Value
				for _, kv := range dp.Attributes.ToSlice() {
					if v := kv.Value.String(); v == "user-7f3c" || v == "very-secret-group" || v == "client-9d1" {
						t.Errorf("attribute %s carries the caller: %q", kv.Key, v)
					}
				}
			}
		}
	}
	return out
}

func TestAuthorizerDecisionsAreCountedWithoutTheCaller(t *testing.T) {
	ctx := context.Background()
	roles, err := auth.NewRoleAuthorizer([]contracts.Grant{{Group: "very-secret-group", Role: contracts.RoleRead}})
	if err != nil {
		t.Fatal(err)
	}
	a := instrument.Authorizer(roles, "roles")
	caller := contracts.Caller{Subject: "user-7f3c", Groups: []string{"very-secret-group"}}
	client := contracts.Caller{ClientID: "client-9d1", Groups: []string{"very-secret-group"}}
	before := authzCounts(t)
	for _, req := range []contracts.Request{
		{Caller: caller, Method: contracts.MethodGet},
		{Caller: caller, Method: contracts.MethodSyncRequest, Source: "github-acme"},
		{Caller: client, Method: contracts.MethodOwner},
		// A caller chooses these names, so they must not become labels.
		{Caller: caller, Method: "query.made-up-by-a-caller-1"},
		{Caller: caller, Method: "query.made-up-by-a-caller-2"},
	} {
		if _, err := a.Authorize(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	got := authzCounts(t)
	for key, want := range map[string]int64{"query.get/allowed": 1, "query.owner/denied": 1, "sync.request/denied": 1, "unlisted/denied": 2} {
		if d := got[key] - before[key]; d != want {
			t.Errorf("%s: counted %d, want %d (all: %v)", key, d, want, got)
		}
	}
	for key := range got {
		method, _, _ := strings.Cut(key, "/")
		if _, listed := contracts.RequiredRole(contracts.Method(method)); !listed && method != "unlisted" {
			t.Errorf("series %q: a method name chosen by a caller became a label", key)
		}
	}
}

func TestAuthorizerBackendFailureMarksTheSpanAndCounts(t *testing.T) {
	a := instrument.Authorizer(failingAuthorizer{}, "rebac")
	before := authzCounts(t)["query.get/error"]
	d, err := a.Authorize(context.Background(), contracts.Request{Method: contracts.MethodGet})
	if err == nil || d.Allowed {
		t.Fatalf("got %+v, %v, want a refusal and the error", d, err)
	}
	if got := authzCounts(t)["query.get/error"] - before; got != 1 {
		t.Errorf("counted %d errors, want 1", got)
	}
	ended := spans.Ended()
	if sp := ended[len(ended)-1]; sp.Name() != "authz.authorize" || sp.Status().Code != codes.Error {
		t.Errorf("got span %q with status %v, want authz.authorize marked as an error", sp.Name(), sp.Status())
	}
}

type contradictoryAuthorizer struct{}

func (contradictoryAuthorizer) Authorize(context.Context, contracts.Request) (contracts.AuthDecision, error) {
	return contracts.AuthDecision{Allowed: true, Role: contracts.RoleAdmin}, errors.New("half-written decision")
}

func TestAuthorizerErrorIsAlwaysADenial(t *testing.T) {
	d, err := instrument.Authorizer(contradictoryAuthorizer{}, "rebac").Authorize(context.Background(), contracts.Request{Method: contracts.MethodGet})
	if err == nil || d.Allowed || d.Role != "" {
		t.Fatalf("got %+v, %v, want an empty denial with the error", d, err)
	}
}

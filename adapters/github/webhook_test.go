package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

// TestStoryWebhooksMatchSync plays the fictional org's story. After each
// change the deliveries the fake sends are handled, and every observation
// they give must be exactly what a full sync at that moment reports for
// the entity (or a deletion, for what the sync no longer lists).
func TestStoryWebhooksMatchSync(t *testing.T) {
	r := newRig(t)
	first, _ := r.sync()
	deletedKeys := map[string]bool{}
	var webhooks adapter.Observations
	var final adapter.Observations
	for _, step := range fakes.Story() {
		if step.Apply == nil {
			continue
		}
		r.clock.Set(step.At)
		if err := step.Apply(r.org); err != nil {
			t.Fatal(err)
		}
		deliveries := r.handleNew()
		if len(deliveries) == 0 {
			t.Fatalf("%s: no deliveries", step.Name)
		}
		fresh, _ := r.sync()
		final = fresh
		listed := byKey(fresh)
		for _, obs := range deliveries {
			webhooks = append(webhooks, obs...)
			for _, o := range obs {
				key := o.GetData().GetEntity().GetKey()
				switch want, ok := listed[key]; {
				case o.GetData().GetEntity().GetDeleted():
					deletedKeys[key] = true
					if ok {
						t.Errorf("%s: %s is deleted by the webhook but listed by the sync", step.Name, key)
					}
				case !ok:
					t.Errorf("%s: webhook observed %s, which the sync does not list", step.Name, key)
				case !proto.Equal(o, want):
					t.Errorf("%s: webhook observation of %s differs from the sync's:\n%v\n%v", step.Name, key, o, want)
				}
			}
		}
	}
	checkGolden(t, "handle-story.golden.jsonl", webhooks)
	checkGolden(t, "sync-after-story.golden.jsonl", final)
	checkDeclared(t, webhooks)

	// legacy-ops was deleted: its node ID, as the first sync saw it.
	legacyOps := get(t, first, "github:team/acme/legacy-ops").GetEntity().GetKey()
	if !deletedKeys[legacyOps] || len(deletedKeys) != 1 {
		t.Errorf("deleted keys = %v, want only legacy-ops %s", deletedKeys, legacyOps)
	}
	payments := get(t, final, "github:repo/acme/payments")
	if got := links(payments); len(got) != 1 || !strings.HasPrefix(got[0], "approves_changes to github:team/acme/platform .github/CODEOWNERS:2") {
		t.Errorf("payments after the story: %q, want only platform approving", got)
	}
	if reliability := get(t, final, "github:team/acme/reliability").GetEntity(); reliability.GetAttributes()["name"].GetStringValue() != "Reliability" {
		t.Errorf("renamed team = %v", reliability)
	}
}

func signedHeaders(event string, body []byte) map[string][]string {
	return map[string][]string{"X-GitHub-Event": {event}, "X-Hub-Signature-256": {fakes.Sign(testSecret, body)}}
}

func (r *rig) handle(event string, body []byte) (adapter.HandleResult, error) {
	return r.a.Handle(context.Background(), adapter.HandleParams{Config: r.cfg, Headers: signedHeaders(event, body), Body: body})
}

func TestHandleIgnoresDeliveriesThatChangeNothingItReads(t *testing.T) {
	const org = `"organization":{"login":"acme","id":81234567}`
	push := func(ref string, files ...string) string {
		b, _ := json.Marshal(files)
		return fmt.Sprintf(`{"ref":%q,"commits":[{"modified":%s}],"repository":{"id":525776495,"default_branch":"main"},%s}`, ref, b, org)
	}
	tests := map[string]struct{ event, body string }{
		"ping":                      {"ping", `{"zen":"Keep it logically awesome."}`},
		"another organization":      {"repository", `{"action":"deleted","repository":{"id":525776495},"organization":{"login":"other","id":1}}`},
		"no organization":           {"repository", `{"action":"deleted","repository":{"id":525776495}}`},
		"push of other files":       {"push", push("refs/heads/main", "README.md", "docs/guide.md")},
		"push to another branch":    {"push", push("refs/heads/feature", ".github/CODEOWNERS")},
		"membership in another org": {"membership", `{"team":{"id":1920002},"organization":{"login":"other","id":1}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			res, err := r.handle(tt.event, []byte(tt.body))
			if err != nil || len(res.Observations) != 0 {
				t.Fatalf("got %v, %v; want no observations and no error", res.Observations, err)
			}
			if n := len(r.srv.Requests()); n != 0 {
				t.Fatalf("sent %d requests to GitHub, want none", n)
			}
		})
	}

	r := newRig(t)
	res, err := r.handle("push", []byte(push("refs/heads/main", "docs/CODEOWNERS")))
	if err != nil || len(res.Observations) != 1 || res.Observations[0].GetData().GetEntity().GetKind() != string(model.KindRepository) {
		t.Fatalf("push of a CODEOWNERS file: got %v, %v; want one repository observation", res.Observations, err)
	}
}

func TestHandleObservesAMissingObjectAsDeleted(t *testing.T) {
	r := newRig(t)
	r.clock.Set(fakes.TeamDeletedAt)
	res, err := r.handle("repository", []byte(`{"action":"deleted","repository":{"id":999},"organization":{"login":"acme","id":81234567}}`))
	if err != nil || len(res.Observations) != 1 {
		t.Fatalf("got %v, %v", res.Observations, err)
	}
	o := res.Observations[0]
	// observed_at is the re-read's send time, not anything in the payload.
	if e := o.GetData().GetEntity(); e.GetKey() != "github:repo_node/"+nextNodeID("R", 999) || !e.GetDeleted() || !o.GetTime().AsTime().Equal(fakes.TeamDeletedAt) {
		t.Fatalf("got %v at %v, want repo 999 deleted at %v", e, o.GetTime().AsTime(), fakes.TeamDeletedAt)
	}
}

func TestHandleRejectsWhatItCannotTrust(t *testing.T) {
	r := newRig(t)
	body := []byte(`{"action":"deleted","repository":{"id":525776495},"organization":{"login":"acme","id":81234567}}`)
	invalid := func(name string, p adapter.HandleParams, want string) {
		t.Helper()
		_, err := r.a.Handle(context.Background(), p)
		var rpcErr *adapter.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeInvalidParams || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an invalid-params error containing %q", name, err, want)
		}
	}
	invalid("bad signature", adapter.HandleParams{
		Config: r.cfg, Body: body,
		Headers: map[string][]string{"X-GitHub-Event": {"repository"}, "X-Hub-Signature-256": {fakes.Sign("wrong", body)}},
	}, "signature")
	invalid("missing signature", adapter.HandleParams{
		Config: r.cfg, Body: body,
		Headers: map[string][]string{"X-GitHub-Event": {"repository"}},
	}, "signature")
	invalid("no secret", adapter.HandleParams{
		Config: json.RawMessage(`{"org":"acme","webhook_secret_env":"NOPE"}`), Body: body,
		Headers: signedHeaders("repository", body),
	}, "NOPE")
	for name, tt := range map[string]struct{ event, body string }{
		"malformed json":  {"repository", `{`},
		"no repository":   {"repository", `{"organization":{"login":"acme","id":1}}`},
		"no team":         {"team", `{"organization":{"login":"acme","id":1}}`},
		"no organization": {"membership", `{"team":{"id":5},"organization":{"login":"acme"}}`},
	} {
		invalid(name, adapter.HandleParams{Config: r.cfg, Body: []byte(tt.body), Headers: signedHeaders(tt.event, []byte(tt.body))}, "")
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Errorf("rejected deliveries sent %d requests to GitHub, want none", n)
	}
}

func TestHandleDoesNotInferDeletionFromAnotherEvent(t *testing.T) {
	r := newRig(t)
	_, err := r.handle("repository", []byte(`{"action":"created","repository":{"id":999},"organization":{"login":"acme","id":81234567}}`))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want an error: a created event for an unreadable repository is not a deletion", err)
	}
	_, err = r.handle("membership", []byte(`{"action":"removed","team":{"id":999},"organization":{"login":"acme","id":81234567}}`))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want an error for a team that can't be read", err)
	}
}

func TestHandleFailsWhenTheReReadFails(t *testing.T) {
	r := newRig(t)
	r.srv.RateLimitNext(1)
	res, err := r.handle("repository", []byte(`{"action":"edited","repository":{"id":525776495},"organization":{"login":"acme","id":81234567}}`))
	if err == nil || !strings.Contains(err.Error(), "rate limit") || len(res.Observations) != 0 {
		t.Fatalf("got %v, %v; want a rate limit error and no observations", res.Observations, err)
	}
}

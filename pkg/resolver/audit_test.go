package resolver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// audited resolves ev and applies it, and returns the audit entries the
// resolver put in its ChangeSet.
func (e *env) audited(ev Event) []*modelv1alpha1.AuditEntry {
	e.t.Helper()
	res, err := e.r.Resolve(context.Background(), ev)
	if err != nil {
		e.t.Fatalf("resolve %s: %v", ev.ID, err)
	}
	e.apply(ev)
	return res.ChangeSet.GetAudit()
}

// entriesOf returns the entries with an action.
func entriesOf(entries []*modelv1alpha1.AuditEntry, action modelv1alpha1.AuditAction) []*modelv1alpha1.AuditEntry {
	var out []*modelv1alpha1.AuditEntry
	for _, a := range entries {
		if a.GetAction() == action {
			out = append(out, a)
		}
	}
	return out
}

// only returns the one entry with an action, or fails.
func only(t testing.TB, entries []*modelv1alpha1.AuditEntry, action modelv1alpha1.AuditAction) *modelv1alpha1.AuditEntry {
	t.Helper()
	got := entriesOf(entries, action)
	if len(got) != 1 {
		t.Fatalf("got %d %s entries in %v, want one", len(got), action, entries)
	}
	return got[0]
}

// auditFor returns the entry with an action whose target is id.
func auditFor(t testing.TB, entries []*modelv1alpha1.AuditEntry, action modelv1alpha1.AuditAction, id string) *modelv1alpha1.AuditEntry {
	t.Helper()
	for _, a := range entriesOf(entries, action) {
		if a.GetTarget().GetId() == id {
			return a
		}
	}
	t.Fatalf("no %s entry for %s in %v", action, id, entries)
	return nil
}

func unpack[M proto.Message](t testing.TB, a interface{ UnmarshalTo(proto.Message) error }, into M) M {
	t.Helper()
	if err := a.UnmarshalTo(into); err != nil {
		t.Fatal(err)
	}
	return into
}

func wantResolverActor(t testing.TB, a *modelv1alpha1.AuditEntry) {
	t.Helper()
	if a.GetActor().GetKind() != modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM || a.GetActor().GetId() != "system:resolver" {
		t.Errorf("%s entry: got actor %v, want the resolver", a.GetAction(), a.GetActor())
	}
}

func TestMintsAreAudited(t *testing.T) {
	e := newEnv(t)
	ev := event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/s1"))
	got := e.audited(ev)

	mine := auditFor(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, "new:e")
	if mine.GetRule() != "observation" || mine.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT {
		t.Errorf("got %v, want the entity's mint by the observation rule, on a subject", mine)
	}
	wantResolverActor(t, mine)
	s := unpack(t, mine.GetAfter(), &modelv1alpha1.Subject{})
	if s.GetKind() != "Repository" || s.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE || s.GetMintedBy().GetEventId() != ev.ID ||
		s.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_OBSERVATION || mine.GetBefore() != nil {
		t.Errorf("got after %v and before %v, want the active Repository this event minted, and no before", s, mine.GetBefore())
	}

	placeholder := auditFor(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, "new:p0")
	if placeholder.GetRule() != "reference" || unpack(t, placeholder.GetAfter(), &modelv1alpha1.Subject{}).GetKind() != "Team" {
		t.Errorf("got %v, want the placeholder team's mint by the reference rule", placeholder)
	}
}

func TestBindingChangesAreAudited(t *testing.T) {
	e := newEnv(t)
	first := e.audited(event("github-acme", fixture(t, "1-repository-codeowners.json")))
	id := "github:repo_node/R_kgDOH1a2bw"
	written := auditFor(t, first, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, id)
	if written.GetBefore() != nil || written.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS {
		t.Errorf("got %v, want the new id alias written with no before", written)
	}
	bt := unpack(t, written.GetAfter(), &modelv1alpha1.BindingTimeline{})
	if bt.GetAlias() != id || len(bt.GetBindings()) != 1 || bt.GetBindings()[0].GetSubjectId() != "new:e" {
		t.Errorf("got after %v, want the alias bound to the new subject", bt)
	}
	auditFor(t, first, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "github:repo/acme/payments-api")

	// The rename releases the old name, which now redirects, and writes the
	// new one.
	second := e.audited(event("github-acme", fixture(t, "2-repository-rename.json")))
	released := auditFor(t, second, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_RELEASED, "github:repo/acme/payments-api")
	before := unpack(t, released.GetBefore(), &modelv1alpha1.BindingTimeline{})
	after := unpack(t, released.GetAfter(), &modelv1alpha1.BindingTimeline{})
	if len(before.GetBindings()) == 0 || before.GetBindings()[0].GetRecordedAt() != nil || !hasReleased(after) || hasReleased(before) {
		t.Errorf("got before %v and after %v, want the open name before, a released row after and no record times", before, after)
	}
	auditFor(t, second, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "github:repo/acme/payments")
	// The id alias did not change, so it has no entry.
	for _, a := range entriesOf(second, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN) {
		if a.GetTarget().GetId() == id {
			t.Errorf("got an entry for the unchanged id alias: %v", a)
		}
	}
}

func hasReleased(bt *modelv1alpha1.BindingTimeline) bool {
	for _, b := range bt.GetBindings() {
		if b.GetReleased() {
			return true
		}
	}
	return false
}

func TestMergesAreAudited(t *testing.T) {
	cfg := testConfig(t)
	for _, d := range cfg.Declarations {
		for _, k := range d.GetKinds() {
			if d.GetName() == "github" && k.GetKind() == "Repository" {
				k.Fields = append(k.Fields, &modelv1alpha1.FieldDeclaration{Predicate: string(model.RelOwnedBy)})
			}
		}
	}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")))
	e.apply(event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "owned_by", "github:team/acme/old")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	placeholder := e.resolveKey("github:team/acme/old", ts("2026-10-03T00:00:00Z"))
	ev := event("github-acme", obsAt("2026-10-01T12:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old"))
	got := e.audited(ev)

	m := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE)
	rec := unpack(t, m.GetAfter(), &modelv1alpha1.MergeRecord{})
	if m.GetRule() != "placeholder" || m.GetConfidencePpm() != 1_000_000 || m.GetTarget().GetId() != team ||
		rec.GetSurvivorId() != team || rec.GetMergedId() != placeholder || rec.GetEventId() != ev.ID || m.GetBefore() != nil {
		t.Errorf("got %v with record %v, want the placeholder merge into %s of %s", m, rec, team, placeholder)
	}
	wantResolverActor(t, m)

	// The merge also changed what the repository's owned_by fact is:
	// its object was a placeholder nobody had observed, and is now the team.
	var changed *modelv1alpha1.AuditEntry
	for _, a := range entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED) {
		if unpack(t, a.GetAfter(), &modelv1alpha1.FactTimeline{}).GetPredicate() == "owned_by" {
			changed = a
		}
	}
	if changed == nil {
		t.Fatalf("got no status change for the fact the merge moved, in %v", got)
	}
	was := unpack(t, changed.GetBefore(), &modelv1alpha1.FactTimeline{}).GetSpans()
	now := unpack(t, changed.GetAfter(), &modelv1alpha1.FactTimeline{}).GetSpans()
	if was[len(was)-1].GetStatus() != modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE || now[len(now)-1].GetStatus() != modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED {
		t.Errorf("got spans %v then %v, want a candidate fact that the merge makes asserted", was, now)
	}
}

// Two subjects that agree with themselves can disagree once they are one: the
// merge opens a conflict, and the audit says so beside the merge.
func TestMergeThatOpensAConflictIsAudited(t *testing.T) {
	cfg := testConfig(t)
	cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, true))
	cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
	e.apply(event("catalog-acme", withAttr(obsAt("2026-10-01T01:00:00Z", "Repository", "catalog:repo_id/C1"), "default_branch", "master")))
	// One observation names both: the catalog's repository is the GitHub one.
	got := e.audited(event("catalog-acme", obsAt("2026-10-01T02:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo_node/R1")))
	only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE)
	opened := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED)
	if n := len(unpack(t, opened.GetAfter(), &modelv1alpha1.Conflict{}).GetPositions()); n != 2 {
		t.Errorf("got a conflict of %d positions, want main against master", n)
	}
	if n := len(entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED)); n == 0 {
		t.Errorf("got no status change in %v, want the facts the conflict made conflicted", got)
	}
}

// An alias has no length limit, an audit target has. The entry must not turn a
// valid event into a rejection.
func TestLongAliasDoesNotMakeAnEventUnappliable(t *testing.T) {
	e := newEnv(t)
	long := "github:repo/acme/" + strings.Repeat("x", 3*contracts.MaxAuditIDBytes)
	ev := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", long))
	res, err := e.r.Resolve(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejections) != 0 {
		t.Fatalf("got rejections %v, want the event to apply", res.Rejections)
	}
	var seen bool
	for _, a := range entriesOf(res.ChangeSet.GetAudit(), modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN) {
		id := a.GetTarget().GetId()
		if strings.HasPrefix(id, "github:repo/acme/xxx") {
			seen = true
			if len(id) > contracts.MaxAuditIDBytes || !strings.Contains(id, "#") {
				t.Errorf("got a target of %d bytes, want a cut one with a hash", len(id))
			}
			if bt := unpack(t, a.GetAfter(), &modelv1alpha1.BindingTimeline{}); bt.GetAlias() != long {
				t.Errorf("got alias of %d bytes in after, want the whole alias", len(bt.GetAlias()))
			}
		}
	}
	if !seen {
		t.Fatal("got no binding entry for the long alias")
	}
	e.apply(ev)
}

func TestFactStatusChangesAreAudited(t *testing.T) {
	e := newEnv(t)
	repo, team := "github:repo_node/R1", "github:team_node/T1"
	// A fact of a subject the event mints has no ID yet; one of a known subject
	// does.
	first := e.audited(event("github-acme", withRelation(withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", repo, "github:repo/acme/a"), "default_branch", "main"), "approves_changes", team)))
	exists := auditFor(t, first, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, "new:e/default_branch")
	if exists.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE || exists.GetBefore() != nil || exists.GetRule() != "" {
		t.Errorf("got %v, want a new asserted fact of a minted subject, by subject and predicate, with no before and no rule", exists)
	}
	ft := unpack(t, exists.GetAfter(), &modelv1alpha1.FactTimeline{})
	if len(ft.GetSpans()) != 1 || ft.GetSpans()[0].GetStatus() != modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED || ft.GetPredicate() != "default_branch" {
		t.Errorf("got after %v, want one asserted span", ft)
	}
	auditFor(t, first, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, "new:e/approves_changes")

	// An observation of a known subject names its facts by their IDs.
	subject := e.resolveKey(repo, time.Time{})
	low := uint32(500_000)
	o := withClaim(obsAt("2026-10-02T00:00:00Z", "Repository", repo), "language", "Go", "", false)
	o.Data.AttributeClaims[0].ConfidencePpm = &low
	second := e.audited(event("github-acme", o))
	var lang *modelv1alpha1.AuditEntry
	for _, a := range entriesOf(second, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED) {
		if a.GetTarget().GetKind() == modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_FACT {
			lang = a
		}
	}
	if lang == nil {
		t.Fatalf("got no fact-targeted status change in %v", second)
	}
	langFT := unpack(t, lang.GetAfter(), &modelv1alpha1.FactTimeline{})
	want, err := model.FactID(subject, "language", langFT.GetObject())
	if err != nil {
		t.Fatal(err)
	}
	if lang.GetTarget().GetId() != want || lang.GetRule() != "below_threshold" || langFT.GetSubjectId() != subject ||
		langFT.GetSpans()[0].GetStatus() != modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE {
		t.Errorf("got %v, want the candidate language fact %s, below the threshold", lang, want)
	}
	wantResolverActor(t, lang)
}

func TestRetractedFactIsAudited(t *testing.T) {
	e := newEnv(t)
	e.apply(syncOf(1, "T1", "T2"))
	got := e.audited(syncOf(2, "T1"))
	var gone []*modelv1alpha1.AuditEntry
	for _, a := range entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED) {
		spans := unpack(t, a.GetAfter(), &modelv1alpha1.FactTimeline{}).GetSpans()
		if len(spans) > 0 && spans[len(spans)-1].GetValidTo() != nil {
			gone = append(gone, a)
		}
	}
	if len(gone) != 1 {
		t.Fatalf("got %d status changes that end a fact's valid time in %v, want the one for T2", len(gone), got)
	}
	before := unpack(t, gone[0].GetBefore(), &modelv1alpha1.FactTimeline{})
	if gone[0].GetRule() != "no_support" || before.GetPredicate() != "approves_changes" || len(before.GetSpans()) == 0 {
		t.Errorf("got %v, want the end of an approves_changes fact for no_support, with its spans before", gone[0])
	}
}

func TestWithdrawnClaimIsAudited(t *testing.T) {
	e := newEnv(t)
	e.apply(syncOf(1, "T1", "T2"))
	got := e.audited(syncOf(2, "T1"))
	w := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_CLAIM_WITHDRAWN)
	was := unpack(t, w.GetBefore(), &modelv1alpha1.SupportTimeline{})
	now := unpack(t, w.GetAfter(), &modelv1alpha1.SupportTimeline{})
	if w.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_FACT || was.GetSource() != "github-acme" || was.GetPredicate() != "approves_changes" ||
		len(was.GetVersions()) != 1 || was.GetVersions()[0].GetValidTo() != nil || was.GetVersions()[0].GetRecordedAt() != nil ||
		len(now.GetVersions()) != 1 || now.GetVersions()[0].GetValidTo() == nil {
		t.Errorf("got %v, want the open claim of github-acme before and its ended claim after, with no record times", w)
	}
	wantResolverActor(t, w)
	// The claim that still stands is not withdrawn.
	if n := len(entriesOf(e.audited(syncOf(3, "T1")), modelv1alpha1.AuditAction_AUDIT_ACTION_CLAIM_WITHDRAWN)); n != 0 {
		t.Errorf("got %d withdrawals for a sync that repeats its claims", n)
	}
}

func TestConflictsOpenedAndClosedAreAudited(t *testing.T) {
	cfg := testConfig(t)
	cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, true))
	cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))

	// Two authoritative systems disagree, so the conflict stands.
	got := e.audited(event("catalog-acme", withAttr(obsAt("2026-10-01T01:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
	repo := e.resolveKey("github:repo_node/R1", time.Time{})
	opened := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED)
	c := unpack(t, opened.GetAfter(), &modelv1alpha1.Conflict{})
	if opened.GetTarget().GetId() != repo+"/default_branch" || opened.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE ||
		len(c.GetPositions()) != 2 || c.GetResolution() != modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_UNSPECIFIED || opened.GetBefore() != nil {
		t.Errorf("got %v with conflict %v, want a standing conflict of two positions on %s/default_branch", opened, c, repo)
	}
	// The facts' statuses changed with it.
	if n := len(entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED)); n != 2 {
		t.Errorf("got %d status changes, want the main and master facts", n)
	}

	// The catalog changes its mind: the supports stop disagreeing.
	got = e.audited(event("catalog-acme", withAttr(obsAt("2026-10-01T02:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "main")))
	closed := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_CLOSED)
	was := unpack(t, closed.GetBefore(), &modelv1alpha1.Conflict{})
	if closed.GetRule() != "evidence_changed" || closed.GetTarget().GetId() != repo+"/default_branch" || len(was.GetPositions()) != 2 || closed.GetAfter() != nil {
		t.Errorf("got %v with conflict %v, want the standing conflict closed by evidence_changed", closed, was)
	}
	if n := len(entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED)); n != 0 {
		t.Errorf("got %d conflicts opened, want none", n)
	}
}

func TestConflictDecidedByAuthorityIsNotOpened(t *testing.T) {
	cfg := testConfig(t)
	cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, false))
	cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
	got := e.audited(event("catalog-acme", withAttr(obsAt("2026-10-01T01:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
	if n := len(entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED)); n != 0 {
		t.Errorf("got %d conflicts opened, want none: authority decided it without it standing", n)
	}
	changed := entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED)
	if len(changed) == 0 {
		t.Fatalf("got no status changes in %v, want the master fact to become a candidate by authority", got)
	}
	rules := ""
	for _, a := range changed {
		rules += a.GetRule() + " "
	}
	if !strings.Contains(rules, "authority") {
		t.Errorf("got rules %q, want authority", rules)
	}
}

func TestRejectionsAreAudited(t *testing.T) {
	e := newEnv(t)
	// A claim outside the subject's domain is rejected; the rest applies.
	ev := event("github-acme", model.NewObservation("adapter/test", ts("2026-10-01T00:00:00Z"), relData("Team", "github:team_node/T1", "approves_changes", "github:team/acme/x")))
	got := e.audited(ev)
	r := only(t, got, modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION)
	if r.GetRejectionCode() != modelv1alpha1.RejectionCode_REJECTION_CODE_DOMAIN_MISMATCH || r.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_EVENT ||
		r.GetTarget().GetId() != ev.ID || !strings.Contains(r.GetReason(), "claim rejected") || !strings.Contains(r.GetReason(), "data.relations[0].type") {
		t.Errorf("got %v, want a domain_mismatch rejection of the claim, on the event", r)
	}
	wantResolverActor(t, r)
	if entriesOf(got, modelv1alpha1.AuditAction_AUDIT_ACTION_MINT)[0].GetTarget().GetId() != "new:e" {
		t.Errorf("got %v, want the rest of the event audited", got)
	}

	// A whole observation can be rejected: an unregistered kind. The event is
	// still recorded, with only its rejection.
	bad := event("github-acme", model.NewObservation("adapter/test", ts("2026-10-02T00:00:00Z"), relData("Spaceship", "github:team_node/T9", "approves_changes", "github:team/acme/x")))
	got = e.audited(bad)
	if len(got) < 1 || got[0].GetAction() != modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION {
		t.Fatalf("got %v, want rejections only", got)
	}
	for _, a := range got {
		if a.GetAction() != modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION || a.GetTarget().GetId() != bad.ID {
			t.Errorf("got %v, want only rejections of the event", a)
		}
	}
}

func TestEventOverTheLimitsAuditsItsRejection(t *testing.T) {
	e := newEnv(t)
	ev := coReportedPeople(e, contracts.MaxChangeSetMerges+2)
	res, err := e.r.Resolve(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	audit := res.ChangeSet.GetAudit()
	if len(audit) != 1 || audit[0].GetRejectionCode() != modelv1alpha1.RejectionCode_REJECTION_CODE_TOO_LARGE || audit[0].GetTarget().GetId() != ev.ID ||
		!strings.Contains(audit[0].GetReason(), "251 merges") {
		t.Fatalf("got %v, want the one too_large rejection that names the merges, and none of the change it refused", audit)
	}
	if len(res.ChangeSet.GetMerges()) != 0 {
		t.Errorf("got %d merges in the ChangeSet that changes nothing", len(res.ChangeSet.GetMerges()))
	}
	if _, err := e.store.Apply(context.Background(), res.ChangeSet); err != nil {
		t.Fatalf("the store refused the rejection's ChangeSet: %v", err)
	}
}

// Syncs that say what the last said, and a repeated delivery, audit nothing.
func TestUnchangedSyncsAddNoAuditEntries(t *testing.T) {
	e := newEnv(t)
	e.apply(syncOf(1, "T1"))
	for h := 2; h < 6; h++ {
		if got := e.audited(syncOf(h, "T1")); len(got) != 0 {
			t.Fatalf("sync %d: got %d audit entries %v, want none for a confirmation", h, len(got), got)
		}
	}
}

// The entries are a function of the event and the store.
func TestAuditEntriesRepeatWhenAnEventIsResolvedAgain(t *testing.T) {
	e := newEnv(t)
	e.apply(syncOf(1, "T1", "T2"))
	ev := syncOf(2, "T2", "T3")
	var runs [2][]*modelv1alpha1.AuditEntry
	for i := range runs {
		res, err := e.r.Resolve(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		runs[i] = res.ChangeSet.GetAudit()
	}
	if len(runs[0]) == 0 {
		t.Fatal("got no audit entries, so the test checks nothing")
	}
	if len(runs[0]) != len(runs[1]) {
		t.Fatalf("got %d and %d entries", len(runs[0]), len(runs[1]))
	}
	for i := range runs[0] {
		if !proto.Equal(runs[0][i], runs[1][i]) {
			t.Errorf("entry %d differs between runs:\n%v\n%v", i, runs[0][i], runs[1][i])
		}
	}
}

// The store takes the resolver's entries as they are, replaces their refs and
// writes a record for each.
func TestStoreRecordsTheResolversEntries(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ev := event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "approves_changes", "github:team/acme/s1"))
	res, err := e.r.Resolve(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	want := len(res.ChangeSet.GetAudit())
	if want < 4 {
		t.Fatalf("got %d entries, want at least the mints, bindings and facts", want)
	}
	e.apply(ev)
	records, err := e.store.AuditLog().Query(ctx, contracts.AuditFilter{Limit: contracts.MaxAuditQueryRecords, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != want {
		t.Fatalf("got %d records, want one for each of the %d entries", len(records), want)
	}
	for i, r := range records {
		if r.GetOrdinal() != uint32(i+1) {
			t.Errorf("record %d has ordinal %d", i, r.GetOrdinal())
		}
		if strings.Contains(fmt.Sprint(r.GetEntry()), "new:") {
			t.Errorf("record %d still holds a ref: %v", r.GetSeq(), r.GetEntry())
		}
	}
}

// The audit log hashes the bytes of an entry, so packing a message with a map
// in it (a qualifier is a Struct) must give the same bytes every time.
func TestPackedEntriesHaveStableBytes(t *testing.T) {
	q := map[string]any{}
	for i := range 40 {
		q[fmt.Sprintf("key%02d", i)] = fmt.Sprint(i)
	}
	st, err := structpb.NewStruct(q)
	if err != nil {
		t.Fatal(err)
	}
	support := &modelv1alpha1.Support{Source: "github-acme", Qualifiers: []*structpb.Struct{st}}
	first, err := pack(support)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := pack(support)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(first.GetValue(), again.GetValue()) {
			t.Fatal("got different bytes for the same message")
		}
	}
}

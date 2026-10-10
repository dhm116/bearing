package conformance

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"bearing.example/pkg/contracts"
)

// Authorizer runs the Authorizer conformance suite (docs/spec/contracts.md,
// "Authorizer"). newAuthorizer builds a backend that gives exactly the roles
// the grants describe, or fails with an error that wraps
// contracts.ErrInvalidGrant for grants that break contracts.CheckGrants.
func Authorizer(t *testing.T, newAuthorizer func(t *testing.T, grants []contracts.Grant) (contracts.Authorizer, error)) {
	a := &authzSuite{newAuthorizer: newAuthorizer}
	for _, c := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"Grants that break the rules are refused", a.invalidGrants},
		{"A group with read may call the read methods and nothing else", a.read},
		{"A caller with no grant is denied with a reason", a.noGrant},
		{"A method with no entry is denied, whoever asks", a.unknownMethod},
		{"Ingest covers its sources and no others", a.ingest},
		{"Admin may call every method on every source", a.admin},
		{"A client is known by its client ID alone", a.clients},
		{"A local caller is an administrator and a name is not enough", a.local},
		{"Roles from several grants add up", a.union},
		{"A decision names its role when it allows and keeps the caller out of its reason", a.decisions},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

type authzSuite struct {
	newAuthorizer func(t *testing.T, grants []contracts.Grant) (contracts.Authorizer, error)
}

func (a *authzSuite) build(t *testing.T, grants ...contracts.Grant) contracts.Authorizer {
	t.Helper()
	az, err := a.newAuthorizer(t, grants)
	failIf(t, err != nil, "build with %+v: %v", grants, err)
	return az
}

// can reports the decision for the call, failing on a backend error.
func can(t *testing.T, az contracts.Authorizer, c contracts.Caller, m contracts.Method, source string) contracts.AuthDecision {
	t.Helper()
	d, err := az.Authorize(ctx, contracts.Request{Caller: c, Method: m, Source: source})
	failIf(t, err != nil, "Authorize %s as %+v: %v", m, c, err)
	failIf(t, !d.Allowed && d.Reason == "", "Authorize %s as %+v denied with no reason", m, c)
	return d
}

func person(groups ...string) contracts.Caller {
	return contracts.Caller{Issuer: "https://idp.example", Subject: "user-7f3c", Groups: groups}
}

func (a *authzSuite) invalidGrants(t *testing.T) {
	for name, g := range map[string]contracts.Grant{
		"neither group nor client": {Role: contracts.RoleRead},
		"both group and client":    {Group: "eng", Client: "ci", Role: contracts.RoleRead},
		"no role":                  {Group: "eng"},
		"unknown role":             {Group: "eng", Role: "root"},
		"client admin":             {Client: "ci", Role: contracts.RoleAdmin},
		"ingest with no source":    {Group: "eng", Role: contracts.RoleIngest},
		"ingest with an empty one": {Group: "eng", Role: contracts.RoleIngest, Sources: []string{" "}},
		"read with a source":       {Group: "eng", Role: contracts.RoleRead, Sources: []string{"github-acme"}},
		"admin with a source":      {Group: "eng", Role: contracts.RoleAdmin, Sources: []string{"github-acme"}},
	} {
		_, err := a.newAuthorizer(t, []contracts.Grant{g})
		failIf(t, !errors.Is(err, contracts.ErrInvalidGrant), "%s: got %v, want ErrInvalidGrant", name, err)
	}
	// One bad grant among good ones refuses them all.
	_, err := a.newAuthorizer(t, []contracts.Grant{{Group: "eng", Role: contracts.RoleRead}, {Client: "ci", Role: contracts.RoleAdmin}})
	failIf(t, !errors.Is(err, contracts.ErrInvalidGrant), "good and bad: got %v, want ErrInvalidGrant", err)
}

func (a *authzSuite) read(t *testing.T) {
	az := a.build(t, contracts.Grant{Group: "eng", Role: contracts.RoleRead})
	for _, m := range contracts.Methods() {
		need, _ := contracts.RequiredRole(m)
		d := can(t, az, person("eng"), m, "github-acme")
		failIf(t, d.Allowed != (need == contracts.RoleRead), "%s (needs %s): allowed = %v", m, need, d.Allowed)
	}
}

func (a *authzSuite) noGrant(t *testing.T) {
	az := a.build(t, contracts.Grant{Group: "eng", Role: contracts.RoleAdmin})
	for _, c := range []contracts.Caller{person(), person("sales"), {}, {Issuer: "https://idp.example", ClientID: "ci"}} {
		for _, m := range contracts.Methods() {
			failIf(t, can(t, az, c, m, "github-acme").Allowed, "%+v called %s with no grant", c, m)
		}
	}
	none := a.build(t)
	failIf(t, can(t, none, person("eng"), contracts.MethodGet, "").Allowed, "a backend with no grants allowed a call")
}

func (a *authzSuite) unknownMethod(t *testing.T) {
	az := a.build(t,
		contracts.Grant{Group: "root", Role: contracts.RoleAdmin},
		contracts.Grant{Client: "bot", Role: contracts.RoleRead})
	callers := []contracts.Caller{
		person("root"),
		{Issuer: "https://idp.example", ClientID: "bot"},
		{Local: true, Subject: "local:0"},
	}
	for _, m := range []contracts.Method{"", "query.unlisted", "QUERY.GET", "query.get ", "admin.everything"} {
		_, known := contracts.RequiredRole(m)
		failIf(t, known, "%q is in the table; the test needs a method that is not", m)
		for _, c := range callers {
			failIf(t, can(t, az, c, m, "github-acme").Allowed, "%+v called the unlisted method %q", c, m)
		}
	}
}

func (a *authzSuite) ingest(t *testing.T) {
	az := a.build(t, contracts.Grant{Group: "team-a", Role: contracts.RoleIngest, Sources: []string{"github-acme", "authentik"}})
	c := person("team-a")
	for _, s := range []string{"github-acme", "authentik"} {
		d := can(t, az, c, contracts.MethodSyncRequest, s)
		failIf(t, !d.Allowed || d.Role != contracts.RoleIngest, "sync of %s: got %+v, want allowed as ingest", s, d)
	}
	for _, s := range []string{"github-other", "", "GITHUB-ACME", "github-acme ", "github-acm"} {
		failIf(t, can(t, az, c, contracts.MethodSyncRequest, s).Allowed, "sync of %q was allowed", s)
	}
	for _, m := range contracts.Methods() {
		if need, _ := contracts.RequiredRole(m); need == contracts.RoleRead {
			failIf(t, can(t, az, c, m, "github-acme").Allowed, "ingest alone called %s", m)
		}
	}
}

func (a *authzSuite) admin(t *testing.T) {
	az := a.build(t, contracts.Grant{Group: "platform", Role: contracts.RoleAdmin})
	for _, m := range contracts.Methods() {
		for _, s := range []string{"github-acme", "authentik", "anything", ""} {
			d := can(t, az, person("platform"), m, s)
			failIf(t, !d.Allowed || d.Role != contracts.RoleAdmin, "%s on %q: got %+v, want allowed as admin", m, s, d)
		}
	}
}

func (a *authzSuite) clients(t *testing.T) {
	az := a.build(t,
		contracts.Grant{Client: "agent-1", Role: contracts.RoleRead},
		contracts.Grant{Client: "syncer", Role: contracts.RoleIngest, Sources: []string{"github-acme"}},
		contracts.Grant{Group: "shared-name", Role: contracts.RoleRead})
	agent := contracts.Caller{Issuer: "https://idp.example", ClientID: "agent-1", Subject: "agent-1"}
	failIf(t, !can(t, az, agent, contracts.MethodOwner, "").Allowed, "the read client was denied a query")
	failIf(t, can(t, az, agent, contracts.MethodSyncRequest, "github-acme").Allowed, "a read client requested a sync")

	syncer := contracts.Caller{Issuer: "https://idp.example", ClientID: "syncer"}
	failIf(t, !can(t, az, syncer, contracts.MethodSyncRequest, "github-acme").Allowed, "the ingest client was denied its source")
	failIf(t, can(t, az, syncer, contracts.MethodSyncRequest, "authentik").Allowed, "the ingest client reached another source")
	failIf(t, can(t, az, syncer, contracts.MethodGet, "").Allowed, "the ingest client ran a query")

	// A user whose subject or group spells a client's ID is not that client,
	// and a client ID is not a group.
	failIf(t, can(t, az, contracts.Caller{Issuer: "https://idp.example", Subject: "agent-1"}, contracts.MethodGet, "").Allowed, "a subject named like a client got its role")
	failIf(t, can(t, az, person("agent-1"), contracts.MethodGet, "").Allowed, "a group named like a client got its role")
	failIf(t, can(t, az, contracts.Caller{ClientID: "shared-name"}, contracts.MethodGet, "").Allowed, "a client ID named like a group got its role")
}

func (a *authzSuite) local(t *testing.T) {
	for _, grants := range [][]contracts.Grant{nil, {{Group: "eng", Role: contracts.RoleRead}}} {
		az := a.build(t, grants...)
		for _, m := range contracts.Methods() {
			d := can(t, az, contracts.Caller{Local: true, Subject: "local:1000"}, m, "github-acme")
			failIf(t, !d.Allowed || d.Role != contracts.RoleAdmin, "local caller on %s: got %+v, want allowed as admin", m, d)
		}
		// Only the server sets Local; a token whose subject looks like a local
		// caller is an ordinary caller.
		for _, m := range contracts.Methods() {
			failIf(t, can(t, az, contracts.Caller{Issuer: "https://idp.example", Subject: "local:0"}, m, "github-acme").Allowed, "subject local:0 without Local called %s", m)
		}
	}
}

func (a *authzSuite) union(t *testing.T) {
	az := a.build(t,
		contracts.Grant{Group: "readers", Role: contracts.RoleRead},
		contracts.Grant{Group: "syncers", Role: contracts.RoleIngest, Sources: []string{"github-acme"}},
		contracts.Grant{Group: "more-syncers", Role: contracts.RoleIngest, Sources: []string{"authentik"}})
	c := person("readers", "syncers", "more-syncers", "unrelated")
	failIf(t, !can(t, az, c, contracts.MethodChanges, "").Allowed, "read from one grant was lost")
	for _, s := range []string{"github-acme", "authentik"} {
		failIf(t, !can(t, az, c, contracts.MethodSyncRequest, s).Allowed, "ingest for %s was lost", s)
	}
	failIf(t, can(t, az, c, contracts.MethodSyncRequest, "kubernetes").Allowed, "the grants added up to a source none names")
	// The same groups in another order decide the same way.
	slices.Reverse(c.Groups)
	failIf(t, !can(t, az, c, contracts.MethodSyncRequest, "authentik").Allowed, "the order of groups changed the decision")
}

func (a *authzSuite) decisions(t *testing.T) {
	az := a.build(t, contracts.Grant{Group: "very-secret-group-name", Role: contracts.RoleRead})
	allowed := can(t, az, person("very-secret-group-name"), contracts.MethodGet, "")
	failIf(t, !allowed.Allowed || allowed.Role != contracts.RoleRead, "got %+v, want allowed as read", allowed)

	c := contracts.Caller{Issuer: "https://idp.example", Subject: "user-7f3c", ClientID: "client-9d1", Groups: []string{"other-secret-group"}}
	for _, m := range []contracts.Method{contracts.MethodGet, contracts.MethodSyncRequest, "query.unlisted"} {
		d := can(t, az, c, m, "github-acme")
		failIf(t, d.Allowed || d.Role != "", "%s: got %+v, want a denial with no role", m, d)
		for _, leak := range []string{c.Subject, c.ClientID, c.Issuer, "other-secret-group", "very-secret-group-name"} {
			failIf(t, strings.Contains(d.Reason, leak), "%s: the reason %q names %q", m, d.Reason, leak)
		}
	}
}

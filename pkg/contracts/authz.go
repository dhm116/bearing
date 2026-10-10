package contracts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Role is what a caller may do (ADR 12, threat model C-API-4).
type Role string

// The roles. Admin includes read, and ingest for every Source.
const (
	// RoleRead runs queries.
	RoleRead Role = "read"
	// RoleIngest requests syncs of, and pushes events to, the Sources it is
	// granted for.
	RoleIngest Role = "ingest"
	// RoleAdmin does everything: configuration, audit queries, and read and
	// ingest for every Source.
	RoleAdmin Role = "admin"
)

// Valid reports whether r is a role this version knows.
func (r Role) Valid() bool {
	return r == RoleRead || r == RoleIngest || r == RoleAdmin
}

// Method names an operation the API offers. Every operation has one entry in
// the table below, and a Method with none is denied (C-API-5).
type Method string

// The methods of the API in this version.
const (
	// MethodGet is "what is this subject": bearing get.
	MethodGet Method = "query.get"
	// MethodOwner is "who owns this": bearing owner.
	MethodOwner Method = "query.owner"
	// MethodRelated is "what is it related to": bearing related.
	MethodRelated Method = "query.related"
	// MethodChanges is "what changed": bearing changes.
	MethodChanges Method = "query.changes"
	// MethodSyncRequest asks for a sync of one Source, which appends a
	// SyncRequested event.
	MethodSyncRequest Method = "sync.request"
)

// methodRoles is the one table of C-API-5: the role each method needs. A
// caller needs that role, or admin. RoleIngest methods are for one Source,
// named in the Request.
var methodRoles = map[Method]Role{
	MethodGet:         RoleRead,
	MethodOwner:       RoleRead,
	MethodRelated:     RoleRead,
	MethodChanges:     RoleRead,
	MethodSyncRequest: RoleIngest,
}

// RequiredRole returns the role a method needs, and false for a method that
// has no entry, which no caller may call.
func RequiredRole(m Method) (Role, bool) {
	r, ok := methodRoles[m]
	return r, ok
}

// Methods returns every method with an entry, sorted by name.
func Methods() []Method {
	out := make([]Method, 0, len(methodRoles))
	for m := range methodRoles {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Caller is who is asking, as the authenticator verified it. A backend
// reads the groups and client ID to find the roles and trusts no other
// field of a token (C-IDP-3). It never holds a token.
type Caller struct {
	// Issuer is the OIDC issuer that vouched for the caller; empty for a
	// local caller.
	Issuer string
	// Subject is the token's sub claim, or "local:<uid>" for the socket's
	// accepted caller. Empty for a client-credentials token with no user.
	Subject string
	// ClientID is the token's azp or client_id claim; set for
	// client-credentials tokens, where Subject names no person.
	ClientID string
	// Groups is the value of the configured groups claim.
	Groups []string
	// Local is set only by the server for a caller it accepted on the
	// Unix socket (C-API-1), who is an administrator.
	Local bool
}

// Request is one call to authorize.
type Request struct {
	// Caller is who is asking.
	Caller Caller
	// Method is the operation. A method with no entry in the table is denied.
	Method Method
	// Source is the configured source a RoleIngest method acts on; empty for
	// the others.
	Source string
}

// AuthDecision is an Authorizer's answer.
type AuthDecision struct {
	// Allowed is true when the call may go ahead.
	Allowed bool
	// Role is the role that allowed the call, empty for a denial.
	Role Role
	// Reason says why in words that are safe to log and to show the caller:
	// it never holds a token, a group list or a secret.
	Reason string
}

// Authorizer decides whether a verified caller may call a method (ADR 12).
// It answers who may call Bearing; PolicyDecider answers whether an action on
// the outside world may run, and the two stay separate.
type Authorizer interface {
	// Authorize returns the decision for r. A denial is an AuthDecision with
	// Allowed false and a nil error. The error is for a backend that could
	// not decide (a relationship store that is down), and the caller treats
	// it as a denial.
	Authorize(ctx context.Context, r Request) (AuthDecision, error)
}

// Grant gives a role to one group or one client ID. It is the default
// backend's configuration, and the conformance suite builds every backend
// from the same grants.
type Grant struct {
	// Group is a value of the groups claim. Exactly one of Group and Client
	// is set.
	Group string
	// Client is a client ID (azp or client_id).
	Client string
	// Role is what the group or client may do.
	Role Role
	// Sources are the Sources a RoleIngest grant covers. A RoleIngest grant
	// names at least one; the other roles name none.
	Sources []string
}

// ErrInvalidGrant is returned by CheckGrants and wrapped in its errors.
var ErrInvalidGrant = errors.New("contracts: invalid grant")

// CheckGrants refuses grants that break the rules of C-API-4: each names a
// group or a client but not both, a role this version knows, Sources only
// for RoleIngest and at least one there, and a client is never an admin.
func CheckGrants(grants []Grant) error {
	for i, g := range grants {
		switch {
		case (g.Group == "") == (g.Client == ""):
			return fmt.Errorf("grant %d: %w: name a group or a client, not both and not neither", i, ErrInvalidGrant)
		case !g.Role.Valid():
			return fmt.Errorf("grant %d: %w: unknown role %q", i, ErrInvalidGrant, g.Role)
		case g.Client != "" && g.Role == RoleAdmin:
			return fmt.Errorf("grant %d: %w: a client may not be an admin", i, ErrInvalidGrant)
		case g.Role == RoleIngest && len(g.Sources) == 0:
			return fmt.Errorf("grant %d: %w: ingest needs at least one source", i, ErrInvalidGrant)
		case g.Role != RoleIngest && len(g.Sources) > 0:
			return fmt.Errorf("grant %d: %w: only ingest names sources", i, ErrInvalidGrant)
		case slices.ContainsFunc(g.Sources, func(s string) bool { return strings.TrimSpace(s) == "" }):
			return fmt.Errorf("grant %d: %w: a source name is empty", i, ErrInvalidGrant)
		}
	}
	return nil
}

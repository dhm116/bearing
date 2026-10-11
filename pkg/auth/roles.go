package auth

import (
	"context"
	"fmt"
	"slices"

	"bearing.example/pkg/contracts"
)

// RoleAuthorizer is the default contracts.Authorizer: it maps IdP groups and
// client IDs to roles through a fixed list of grants (threat model C-API-4,
// C-IDP-3). It denies a caller with no grant.
type RoleAuthorizer struct {
	groups  map[string][]contracts.Grant
	clients map[string][]contracts.Grant
}

var _ contracts.Authorizer = (*RoleAuthorizer)(nil)

// NewRoleAuthorizer returns an authorizer for grants, or an error that wraps
// contracts.ErrInvalidGrant if any grant breaks contracts.CheckGrants.
func NewRoleAuthorizer(grants []contracts.Grant) (*RoleAuthorizer, error) {
	if err := contracts.CheckGrants(grants); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	a := &RoleAuthorizer{groups: map[string][]contracts.Grant{}, clients: map[string][]contracts.Grant{}}
	for _, g := range grants {
		if g.Group != "" {
			a.groups[g.Group] = append(a.groups[g.Group], g)
		} else {
			a.clients[g.Client] = append(a.clients[g.Client], g)
		}
	}
	return a, nil
}

// Authorize implements contracts.Authorizer.
func (a *RoleAuthorizer) Authorize(_ context.Context, r contracts.Request) (contracts.AuthDecision, error) {
	need, known := contracts.RequiredRole(r.Method)
	if !known {
		return deny("the method is not one Bearing offers"), nil
	}
	if r.Caller.Local {
		return allow(contracts.RoleAdmin), nil
	}
	// A caller with a client ID is a client and holds only that client's
	// roles. The groups claim of a client's token is ignored, so it cannot
	// lift the client above its mapping (C-API-4).
	var held []contracts.Grant
	if r.Caller.ClientID != "" {
		held = a.clients[r.Caller.ClientID]
	} else {
		for _, g := range r.Caller.Groups {
			held = append(held, a.groups[g]...)
		}
	}
	if len(held) == 0 {
		return deny("the caller has no role"), nil
	}
	for _, g := range held {
		if g.Role == contracts.RoleAdmin {
			return allow(contracts.RoleAdmin), nil
		}
	}
	for _, g := range held {
		switch {
		case g.Role != need:
		case need == contracts.RoleIngest && !slices.Contains(g.Sources, r.Source):
		default:
			return allow(need), nil
		}
	}
	if need == contracts.RoleIngest {
		return deny("the caller may not use this method on this source"), nil
	}
	return deny("the caller's role does not allow this method"), nil
}

func allow(role contracts.Role) contracts.AuthDecision {
	return contracts.AuthDecision{Allowed: true, Role: role, Reason: "allowed as " + string(role)}
}

func deny(reason string) contracts.AuthDecision {
	return contracts.AuthDecision{Reason: reason}
}

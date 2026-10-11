// Package auth answers who may call Bearing (ADR 12). Verifier checks an OIDC
// bearer token against the issuer's keys and builds the contracts.Caller it
// proves; RoleAuthorizer, the default contracts.Authorizer, maps the groups
// and client ID of that caller to roles. Bearing issues no credentials and
// stores none.
package auth

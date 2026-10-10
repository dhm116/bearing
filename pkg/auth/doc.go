// Package auth answers who may call Bearing (ADR 12). It holds the default
// contracts.Authorizer, which maps the groups and client ID a verified
// caller carries to roles. Bearing issues no credentials and stores none:
// what a token proves is checked elsewhere, and this package decides only
// what the proven identity may do.
package auth

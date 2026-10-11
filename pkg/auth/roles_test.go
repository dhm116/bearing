package auth

import (
	"testing"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

func TestRoleAuthorizerPassesConformance(t *testing.T) {
	conformance.Authorizer(t, func(_ *testing.T, grants []contracts.Grant) (contracts.Authorizer, error) {
		return NewRoleAuthorizer(grants)
	})
}

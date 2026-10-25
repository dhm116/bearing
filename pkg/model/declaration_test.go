package model

import (
	"path/filepath"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestReferenceDeclarationsValidate(t *testing.T) {
	files, err := filepath.Glob("../../testdata/declarations/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("got %d declaration fixtures (%v), want some", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			d := &modelv1alpha1.AdapterDeclaration{}
			if err := DecodeJSON(readFixture(t, file), d); err != nil {
				t.Fatal(err)
			}
			if err := ValidateDeclaration(d); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func wantCode(t *testing.T, err error, code modelv1alpha1.RejectionCode) {
	t.Helper()
	ve, ok := err.(*ValidationError) //nolint:errorlint // validators return it unwrapped
	if !ok || !ve.Has(code) {
		t.Fatalf("got %v, want %s", err, ShortName(code))
	}
}

func TestValidateDeclarationRejects(t *testing.T) {
	kind := func(k *modelv1alpha1.KindDeclaration) *modelv1alpha1.AdapterDeclaration {
		return &modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "x", Kinds: []*modelv1alpha1.KindDeclaration{k}}
	}
	id := &modelv1alpha1.KeyTypeDeclaration{KeyType: "id", Class: modelv1alpha1.KeyClass_KEY_CLASS_ID}
	field := func(f *modelv1alpha1.FieldDeclaration) *modelv1alpha1.AdapterDeclaration {
		return kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{id}, Fields: []*modelv1alpha1.FieldDeclaration{f}})
	}
	const hmacScheme = modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256
	withWebhook := func(w *modelv1alpha1.WebhookSignature) *modelv1alpha1.AdapterDeclaration {
		return &modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "x", Webhook: w}
	}
	tests := []struct {
		name string
		d    *modelv1alpha1.AdapterDeclaration
		want modelv1alpha1.RejectionCode
	}{
		{"no name", &modelv1alpha1.AdapterDeclaration{IssuerType: "x"}, codeMalformed},
		{"bad issuer", &modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "X Y"}, codeMalformed},
		{"unknown kind", kind(&modelv1alpha1.KindDeclaration{Kind: "Widget"}), codeNotDeclared},
		{"no class", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{{KeyType: "t"}}}), codeMalformed},
		{"id redirects", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{{KeyType: "t", Class: modelv1alpha1.KeyClass_KEY_CLASS_ID, Redirects: true}}}), codeMalformed},
		{"key twice", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{id, id}}), codeMalformed},
		{"core predicate", field(&modelv1alpha1.FieldDeclaration{Predicate: "same_as"}), codeCorePredicate},
		{"domain", field(&modelv1alpha1.FieldDeclaration{Predicate: "approves_changes"}), codeDomainMismatch},
		{"range", field(&modelv1alpha1.FieldDeclaration{Predicate: "changed_by", Direction: modelv1alpha1.Direction_DIRECTION_IN}), codeDomainMismatch},
		{"attribute domain", field(&modelv1alpha1.FieldDeclaration{Predicate: "login"}), codeDomainMismatch},
		{"unregistered without type", field(&modelv1alpha1.FieldDeclaration{Predicate: "size", Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE}), codeMalformed},
		{"registered with other type", field(&modelv1alpha1.FieldDeclaration{Predicate: "name", Type: modelv1alpha1.ValueType_VALUE_TYPE_BOOL}), codeTypeMismatch},
		{"attribute in", field(&modelv1alpha1.FieldDeclaration{Predicate: "name", Direction: modelv1alpha1.Direction_DIRECTION_IN}), codeMalformed},
		{"members out", field(&modelv1alpha1.FieldDeclaration{Predicate: "member_of", Match: modelv1alpha1.MatchMethod_MATCH_METHOD_MEMBERS}), codeMalformed},
		{"exists", field(&modelv1alpha1.FieldDeclaration{Predicate: "exists"}), codeMalformed},
		{"webhook without a scheme", withWebhook(&modelv1alpha1.WebhookSignature{SignatureHeader: "X-Sig"}), codeMalformed},
		{"webhook without a header", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme}), codeMalformed},
		{"webhook header with a space", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme, SignatureHeader: "X Sig"}), codeMalformed},
		{"webhook prefix with a comma", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme, SignatureHeader: "X-Sig", SignaturePrefix: "v1,"}), codeMalformed},
		{"webhook prefix too long", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme, SignatureHeader: "X-Sig", SignaturePrefix: strings.Repeat("a", MaxSignaturePrefixBytes+1)}), codeMalformed},
		{"webhook scheme the host has no verifier for", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: 99, SignatureHeader: "X-Sig"}), codeMalformed},
		{"webhook scheme below zero", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: -1, SignatureHeader: "X-Sig"}), codeMalformed},
		{"webhook signature in Authorization", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme, SignatureHeader: "authorization"}), codeMalformed},
		{"webhook signature in Content-Length", withWebhook(&modelv1alpha1.WebhookSignature{Scheme: hmacScheme, SignatureHeader: "Content-Length"}), codeMalformed},
		{"bad link", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Links: []*modelv1alpha1.LinkDeclaration{{IssuerType: "saml"}}}), codeMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantCode(t, ValidateDeclaration(tt.d), tt.want)
		})
	}
}

func TestValidateDeclarationAcceptsAWebhook(t *testing.T) {
	for name, w := range map[string]*modelv1alpha1.WebhookSignature{
		"GitHub":    {Scheme: modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256, SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256="},
		"PagerDuty": {Scheme: modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256, SignatureHeader: "X-PagerDuty-Signature", SignaturePrefix: "v1="},
		"bare":      {Scheme: modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256, SignatureHeader: "X-Signature"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDeclaration(&modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "x", Webhook: w}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeclaredAttributesKeepsOnlyUnregisteredFields(t *testing.T) {
	d := &modelv1alpha1.AdapterDeclaration{Kinds: []*modelv1alpha1.KindDeclaration{{
		Fields: []*modelv1alpha1.FieldDeclaration{
			{Predicate: "name"},
			{Predicate: "cluster_zone", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING},
		},
	}}}
	got := DeclaredAttributes(d)
	if len(got) != 1 || got["cluster_zone"].Type != modelv1alpha1.ValueType_VALUE_TYPE_STRING {
		t.Fatalf("got %v, want only cluster_zone, a string", got)
	}
	if n := len(DeclaredAttributes(nil)); n != 0 {
		t.Fatalf("got %d attributes from no declaration, want 0", n)
	}
}

func TestLinkAuthorityVouchesOnlyForDeclaredAuthoritativeLinks(t *testing.T) {
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := DecodeJSON(readFixture(t, "../../testdata/declarations/authentik.json"), d); err != nil {
		t.Fatal(err)
	}
	vouches := LinkAuthority(d)
	for key, want := range map[Key]bool{
		"github:user_node/U_1": true,  // declared authoritative
		"github:user/jane":     false, // declared, not authoritative
		"gitlab:user_node/1":   false, // another issuer
		"not a key":            false,
	} {
		if got := vouches(key); got != want {
			t.Errorf("%s: got %v, want %v", key, got, want)
		}
	}
	if LinkAuthority(nil)("github:user_node/U_1") {
		t.Error("a nil declaration vouched for a key")
	}
}

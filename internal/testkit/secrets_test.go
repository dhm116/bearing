package testkit

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCanary(t *testing.T) {
	a, b := Canary("env:A"), Canary("env:B")
	if a == b || a != Canary("env:A") {
		t.Fatalf("canaries not distinct and deterministic: %q %q", a, b)
	}
	if !strings.HasPrefix(a, "bearing-canary-") || len(a) != len("bearing-canary-")+16 {
		t.Fatalf("unexpected canary shape %q", a)
	}
}

func TestSecretsResolve(t *testing.T) {
	s := NewSecrets()
	s.Set("env:SET", "explicit")
	s.Set("env:EMPTY", "")
	s.Set("env:GONE", "x")
	s.Unset("env:GONE")

	tests := []struct {
		ref     string
		want    string
		wantErr error // nil, ErrSecretNotFound, or errMalformed
	}{
		{"env:GITHUB_TOKEN", Canary("env:GITHUB_TOKEN"), nil},
		{"file:/run/secrets/x", Canary("file:/run/secrets/x"), nil},
		{"vault:kv/bearing#github", Canary("vault:kv/bearing#github"), nil},
		{"aws-sm:arn:aws:secretsmanager:us-east-1:1:secret:x", Canary("aws-sm:arn:aws:secretsmanager:us-east-1:1:secret:x"), nil},
		{"env:SET", "explicit", nil},
		{"env:EMPTY", "", nil},
		{"env:GONE", "", ErrSecretNotFound},
		{"GITHUB_TOKEN", "", errMalformed},
		{"env:", "", errMalformed},
		{"https://example.com/token", "", errMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := s.Resolve(context.Background(), tt.ref)
			switch {
			case errors.Is(tt.wantErr, errMalformed):
				if err == nil || !strings.Contains(err.Error(), "malformed") {
					t.Fatalf("err = %v, want malformed", err)
				}
			case !errors.Is(err, tt.wantErr):
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	// Set after Unset restores the reference.
	s.Set("env:GONE", "back")
	if v, err := s.Resolve(context.Background(), "env:GONE"); err != nil || v != "back" {
		t.Fatalf("after Set: %q, %v", v, err)
	}
}

var errMalformed = errors.New("malformed")

func TestSecretsRecordsResolution(t *testing.T) {
	s := NewSecrets()
	s.Set("file:/b", "bee")
	s.Set("env:A", Canary("env:A"))
	_ = s.Getenv("A")
	_ = s.Getenv("A")
	_, _ = s.Resolve(context.Background(), "file:/b")
	// Getenv, unlike Resolve, returns "" for names never Set.
	for _, name := range []string{"", "NEVER_SET"} {
		if got := s.Getenv(name); got != "" {
			t.Fatalf("Getenv(%q) = %q, want \"\"", name, got)
		}
	}
	s.Set("env:GONE", "x")
	s.Unset("env:GONE")
	if got := s.Getenv("GONE"); got != "" {
		t.Fatalf("Getenv of unset name = %q", got)
	}

	if got, want := s.Resolved(), []string{"env:A", "env:A", "file:/b"}; !slices.Equal(got, want) {
		t.Fatalf("Resolved = %v, want %v", got, want)
	}
	s.Set("file:/b", "changed") // values already handed out are kept
	if got, want := s.Values(), []string{Canary("env:A"), "bee"}; !slices.Equal(got, want) {
		t.Fatalf("Values = %v, want %v", got, want)
	}
}

func TestSecretsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSecrets().Resolve(ctx, "env:A"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSecretsConcurrent(t *testing.T) {
	s := NewSecrets()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				ref := fmt.Sprintf("env:V%d", j%5)
				if i%2 == 0 {
					s.Set(ref, "x")
				}
				_, _ = s.Resolve(context.Background(), ref)
				_ = s.Values()
				_ = s.Resolved()
			}
		}()
	}
	wg.Wait()
	if n := len(s.Resolved()); n != 400 {
		t.Fatalf("Resolved has %d entries, want 400", n)
	}
}

func TestFindLeaks(t *testing.T) {
	const secret = "s3cr3t/t0ken+v>?alue" // base64 contains "+"
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	lower := strings.NewReplacer("%2F", "%2f", "%2B", "%2b", "%3E", "%3e", "%3F", "%3f").Replace
	const jsonSecret = `pa"ss\word<1>&`   //nolint:gosec // G101: fake secret for leak tests
	const quoteSecret = "tok\x01ené-1234" //nolint:gosec // G101: fake secret for leak tests; JSON writes \u0001, Go quoting \x01
	jsonNoHTML := func(s string) string {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(s)
		return strings.TrimSpace(b.String())
	}
	jsonHTML, _ := json.Marshal(jsonSecret)
	tests := []struct {
		name     string
		secret   string // defaults to secret
		data     string
		wantForm string // "" for no leak
	}{
		{"clean", "", "nothing to see", ""},
		{"raw", "", "token=" + secret, "raw"},
		{"query escaped", "", "GET /x?token=" + url.QueryEscape(secret), "url-query"},
		{"query escaped lower-case", "", "GET /x?token=" + lower(url.QueryEscape(secret)), "url-query"},
		{"path escaped", "", "GET /x/" + url.PathEscape(secret), "url-path"},
		{"path escaped lower-case", "", "GET /x/" + lower(url.PathEscape(secret)), "url-path"},
		{"url.full escaped path", "", "url.full=" + (&url.URL{Scheme: "https", Host: "h", Path: "/x/" + secret}).String(), "url-escaped-path"},
		{"escaped path lower-case", "", "/x/" + lower((&url.URL{Path: secret}).EscapedPath()), "url-escaped-path"},
		{"json html-escaped", jsonSecret, `{"token":` + string(jsonHTML) + `}`, "json"},
		{"json slog handler", jsonSecret, `{"token":` + jsonNoHTML(jsonSecret) + `}`, "json"},
		{"go quoted slog text", quoteSecret, "token=" + strconv.Quote(quoteSecret), "quoted"},
		{"base64", "", "auth: " + b64, "base64"},
		{"base64 unpadded", "", "auth: " + strings.TrimRight(b64, "="), "base64"},
		{"base64url", "", "jwt." + base64.RawURLEncoding.EncodeToString([]byte(secret)), "base64url"},
		{"base64 shift 1", "", "Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+secret)), "base64"},
		{"base64 shift 2", "", base64.StdEncoding.EncodeToString([]byte("ab:" + secret + "!")), "base64"},
		{"base64 shift 0 inside", "", base64.StdEncoding.EncodeToString([]byte("abc" + secret)), "base64"},
		{"hex", "", "0x" + hex.EncodeToString([]byte(secret)), "hex"},
		{"HEX", "", strings.ToUpper(hex.EncodeToString([]byte(secret))), "HEX"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := cmp.Or(tt.secret, secret)
			leaks := FindLeaks(tt.data, secret)
			if tt.wantForm == "" {
				if len(leaks) != 0 {
					t.Fatalf("unexpected leaks %+v", leaks)
				}
				return
			}
			if len(leaks) == 0 || leaks[0].Form != tt.wantForm || leaks[0].Secret != 0 {
				t.Fatalf("leaks = %+v, want form %s", leaks, tt.wantForm)
			}
			// The []byte form reports the same.
			if b := FindLeaks([]byte(tt.data), secret); !slices.Equal(b, leaks) {
				t.Fatalf("[]byte leaks %+v != string leaks %+v", b, leaks)
			}
		})
	}
}

func TestFindLeaksDetails(t *testing.T) {
	canary := Canary("env:X") // URL forms equal raw and are reported once
	data := "xx" + canary
	got := FindLeaks(data, "", "unrelated", canary)
	want := []Leak{{Secret: 2, Form: "raw", Offset: 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Short secrets skip base64 fragments that would match by chance.
	if got := FindLeaks("QUJD", "ab"); len(got) != 0 {
		t.Fatalf("short secret matched base64 noise: %+v", got)
	}
}

// fakeTB captures Errorf calls so failure paths can be tested. Servers call
// Errorf from their handler goroutines, hence the lock.
type fakeTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}

// errors returns the messages captured so far.
func (f *fakeTB) errors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.errs)
}

func TestAssertNoLeaks(t *testing.T) {
	canary := Canary("env:TOKEN")
	AssertNoLeaks(t, []byte("all clear"), canary)
	AssertNoLeaks(t, "short secrets are logged, not failed", "ab")

	f := &fakeTB{TB: t}
	AssertNoLeaks(f, `level=INFO msg="calling upstream" auth="Bearer `+canary+`"`, canary)
	if errs := f.errors(); len(errs) != 1 || !strings.Contains(errs[0], "leaked (raw)") || !strings.Contains(errs[0], "Bearer") {
		t.Fatalf("errors = %q", errs)
	}
}

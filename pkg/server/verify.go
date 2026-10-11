package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// maxSignatureCandidates is how many comma-separated values of the signature
// header a delivery is checked against (docs/spec/data-model.md,
// "Declarations").
const maxSignatureCandidates = 8

// hmacSHA256 verifies HMAC-SHA256 signatures over the raw body, as GitHub and
// PagerDuty send them: a header holding one or more candidates, each the
// prefix and 64 hex digits.
type hmacSHA256 struct {
	header string
	prefix string
	key    []byte
}

// verify reports whether one of the candidates in h is the HMAC of body. It
// compares every candidate, in constant time each, so how many were wrong or
// which one matched is not visible in the time taken.
func (v hmacSHA256) verify(h http.Header, body []byte) bool {
	mac := hmac.New(sha256.New, v.key)
	mac.Write(body)
	want := mac.Sum(nil)

	seen, ok := 0, 0
	for _, line := range h.Values(v.header) {
		for _, c := range strings.Split(line, ",") {
			if seen == maxSignatureCandidates {
				return ok == 1
			}
			seen++
			digest, found := strings.CutPrefix(strings.TrimSpace(c), v.prefix)
			if !found || len(digest) != 2*sha256.Size {
				continue
			}
			got, err := hex.DecodeString(digest)
			if err != nil {
				continue
			}
			ok |= subtle.ConstantTimeCompare(got, want)
		}
	}
	return ok == 1
}

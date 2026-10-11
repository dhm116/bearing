package auth

import (
	"errors"
	"net/http"
	"strings"
)

// ErrNoToken means the request carried no Authorization header.
var ErrNoToken = errors.New("auth: no bearer token")

// BearerToken returns the token in the request's Authorization header and
// nowhere else: not a query parameter, a cookie or a second header (C-IDP-1,
// C-IDP-5), since those end up in logs and referrers. It returns ErrNoToken
// when there is no header, and an error wrapping ErrInvalidToken for one that
// is not exactly "Bearer <token>". Only Verify counts tokens in
// bearing.auth.tokens, so a server that refuses a header here counts it as
// "malformed" itself.
func BearerToken(h http.Header) (string, error) {
	values := h.Values("Authorization")
	switch len(values) {
	case 0:
		return "", ErrNoToken
	case 1:
	default:
		return "", refuse("malformed", "more than one Authorization header")
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", refuse("malformed", "the Authorization header is not a bearer token")
	}
	return token, nil
}

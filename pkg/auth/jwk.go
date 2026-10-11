package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// The signature algorithms Bearing accepts (threat model C-IDP-1). "none" and
// the HMAC family are absent on purpose: a symmetric algorithm would let
// anyone who knows a public key forge a token.
const (
	algRS256 = "RS256"
	algPS256 = "PS256"
	algES256 = "ES256"
	algEdDSA = "EdDSA"
)

// The RSA modulus sizes accepted. The upper bound keeps one verification fast:
// a 2^20-bit modulus takes seconds.
const (
	minRSABits = 2048
	maxRSABits = 8192
)

// jwk is one parsed key of a JWK Set (RFC 7517) that Bearing can use.
type jwk struct {
	key crypto.PublicKey
	// alg is the key's own "alg" when it has one, else empty.
	alg string
}

// jwkJSON is the part of a JWK that is read.
type jwkJSON struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS reads a JWK Set into the keys Bearing can verify with, by key ID.
// It skips keys it does not support or that are unusable (an encryption key,
// a short RSA key, a point off the curve), because an identity provider's set
// holds keys for other purposes. A key ID that appears twice is dropped
// entirely, since there is no telling which key signed a token. A key with no
// ID is skipped: tokens must name their key.
func parseJWKS(raw []byte) (map[string]jwk, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("the key set is not JSON with a keys list")
	}
	keys := map[string]jwk{}
	ambiguous := map[string]bool{}
	for _, r := range set.Keys {
		var j jwkJSON
		if err := json.Unmarshal(r, &j); err != nil || j.Kid == "" || (j.Use != "" && j.Use != "sig") {
			continue
		}
		k, err := j.parse()
		if err != nil {
			continue
		}
		if _, dup := keys[j.Kid]; dup || ambiguous[j.Kid] {
			ambiguous[j.Kid] = true
			delete(keys, j.Kid)
			continue
		}
		keys[j.Kid] = k
	}
	return keys, nil
}

func (j jwkJSON) parse() (jwk, error) {
	switch j.Kty {
	case "RSA":
		n, err1 := b64Int(j.N)
		e, err2 := b64Int(j.E)
		if err1 != nil || err2 != nil || n.BitLen() < minRSABits || n.BitLen() > maxRSABits || !e.IsInt64() || e.Int64() < 65537 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
			return jwk{}, errors.New("unusable RSA key")
		}
		if j.Alg != "" && j.Alg != algRS256 && j.Alg != algPS256 {
			return jwk{}, errors.New("RSA key with another algorithm")
		}
		return jwk{key: &rsa.PublicKey{N: n, E: int(e.Int64())}, alg: j.Alg}, nil
	case "EC":
		if j.Crv != "P-256" || (j.Alg != "" && j.Alg != algES256) {
			return jwk{}, errors.New("unsupported EC key")
		}
		x, err1 := base64.RawURLEncoding.Strict().DecodeString(j.X)
		y, err2 := base64.RawURLEncoding.Strict().DecodeString(j.Y)
		if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
			return jwk{}, errors.New("unusable EC key")
		}
		// Parsing checks that the point is on the curve.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append([]byte{4}, append(x, y...)...))
		if err != nil {
			return jwk{}, errors.New("EC point is not on the curve")
		}
		return jwk{key: pub, alg: j.Alg}, nil
	case "OKP":
		if j.Crv != "Ed25519" || (j.Alg != "" && j.Alg != algEdDSA) {
			return jwk{}, errors.New("unsupported OKP key")
		}
		x, err := base64.RawURLEncoding.Strict().DecodeString(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return jwk{}, errors.New("unusable Ed25519 key")
		}
		return jwk{key: ed25519.PublicKey(x), alg: j.Alg}, nil
	}
	return jwk{}, errors.New("unsupported key type")
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad integer")
	}
	return new(big.Int).SetBytes(b), nil
}

// verifySignature checks sig over signed with k under alg. The key's type
// must be the one alg needs, and a key that names an algorithm must name this
// one, so a token cannot choose how its own key is used.
func verifySignature(alg string, k jwk, signed, sig []byte) error {
	if k.alg != "" && k.alg != alg {
		return errors.New("the key is for another algorithm")
	}
	switch alg {
	case algRS256, algPS256:
		pub, ok := k.key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the key is not an RSA key")
		}
		sum := sha256Sum(signed)
		if alg == algRS256 {
			return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum, sig)
		}
		return rsa.VerifyPSS(pub, crypto.SHA256, sum, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case algES256:
		pub, ok := k.key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("the key is not an EC key")
		}
		if len(sig) != 64 {
			return errors.New("bad ES256 signature length")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, sha256Sum(signed), r, s) {
			return errors.New("signature does not match")
		}
		return nil
	case algEdDSA:
		pub, ok := k.key.(ed25519.PublicKey)
		if !ok {
			return errors.New("the key is not an Ed25519 key")
		}
		if !ed25519.Verify(pub, signed, sig) {
			return errors.New("signature does not match")
		}
		return nil
	}
	return fmt.Errorf("algorithm %q is not accepted", alg)
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

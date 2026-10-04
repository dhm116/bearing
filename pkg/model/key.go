package model

import (
	"fmt"
	"regexp"
	"strings"
)

// Key identifies an alias, in the form "<namespace>:<key_type>/<external_id>",
// for example "github:repo_node/R_kgDOH1a2b3". Keys are strings on the wire.
// The core, not adapters, decides when keys name the same subject.
type Key string

// namespace and key_type are lowercase letters, digits and hyphens (key_type
// also "_"); the external ID is the issuer's value and may hold "/" and ":".
var (
	keyPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*:[a-z0-9][a-z0-9_-]*/.+$`)
	namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	keyTypePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Parse splits a key at the first ":" and the first "/" after it.
func (k Key) Parse() (namespace, keyType, externalID string, err error) {
	if !keyPattern.MatchString(string(k)) {
		return "", "", "", fmt.Errorf("invalid key %q: want <namespace>:<key_type>/<external_id>", k)
	}
	namespace, rest, _ := strings.Cut(string(k), ":")
	keyType, externalID, _ = strings.Cut(rest, "/")
	return namespace, keyType, externalID, nil
}

// NewKey builds a key from its parts.
func NewKey(namespace, keyType, externalID string) Key {
	return Key(namespace + ":" + keyType + "/" + externalID)
}

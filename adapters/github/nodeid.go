package github

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
)

// GitHub's next-format global node IDs are a type prefix, "_", and the
// URL-safe unpadded base64 of a MessagePack array: [0, database ID] for
// users and repositories, [0, org ID, team ID] for teams. The legacy format
// is standard base64, which has no "_", so nextID tells them apart. Keys
// hold next-format IDs only (docs/spec/data-model.md, "Key types").

// nextNodeID derives the next-format node ID for prefix and database IDs.
// Webhook payloads carry only legacy IDs, and no header selects the format
// there.
func nextNodeID(prefix string, ids ...int64) string {
	b := []byte{0x90 | byte(len(ids)+1), 0x00} //nolint:gosec // G115: a fixarray holds at most 15; node IDs have two or three
	for _, id := range ids {
		b = appendUint(b, uint64(id)) //nolint:gosec // G115: callers pass positive database IDs
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}

// appendUint appends v in MessagePack's shortest unsigned encoding.
func appendUint(b []byte, v uint64) []byte {
	switch {
	case v < 0x80:
		return append(b, byte(v))
	case v <= 0xff:
		return append(b, 0xcc, byte(v))
	case v <= 0xffff:
		return binary.BigEndian.AppendUint16(append(b, 0xcd), uint16(v))
	case v <= 0xffffffff:
		return binary.BigEndian.AppendUint32(append(b, 0xce), uint32(v))
	default:
		return binary.BigEndian.AppendUint64(append(b, 0xcf), v)
	}
}

var nextIDPattern = regexp.MustCompile(`^[A-Z]{1,4}_[A-Za-z0-9_-]+$`)

// nextID returns id if it is a next-format node ID of the given prefix, and
// an error otherwise, so a legacy ID is never emitted as a key.
func nextID(prefix, id string) (string, error) {
	if !nextIDPattern.MatchString(id) || !strings.HasPrefix(id, prefix+"_") {
		return "", fmt.Errorf("%s node ID is not in the next format (is X-Github-Next-Global-ID: 1 honored?)", prefix)
	}
	return id, nil
}

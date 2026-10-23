package authentik

import (
	"encoding/base64"
	"encoding/binary"
)

// nextNodeID derives GitHub's next-format node ID for prefix and database
// IDs: the prefix, "_", and the URL-safe unpadded base64 of a MessagePack
// array [0, ids...]. It is the same derivation as adapters/github's, which
// keeps it unexported; the OAuth connection records only the numeric ID.
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

package fakes

import (
	"encoding/base64"
	"encoding/binary"
	"strconv"
)

// GitHub global node IDs come in two formats. The next format, which
// GitHub returns when a request carries X-Github-Next-Global-ID: 1, is a
// type prefix, "_", and the URL-safe unpadded base64 of a MessagePack
// array: [0, database ID] for users and repositories, [0, org ID, team ID]
// for teams. The legacy format is the standard base64 of
// "0<len(type)>:<type><database ID>". The spec allows only the next format
// in keys (docs/spec/data-model.md, "Key types").

// nextNodeID returns the next-format node ID for prefix and database IDs.
func nextNodeID(prefix string, ids ...int64) string {
	b := []byte{0x90 | byte(len(ids)+1), 0x00} //nolint:gosec // G115: a fixarray holds at most 15; node IDs have two or three
	for _, id := range ids {
		b = appendMsgpackUint(b, uint64(id)) //nolint:gosec // G115: database IDs are positive
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}

// appendMsgpackUint appends v in MessagePack's shortest unsigned encoding.
func appendMsgpackUint(b []byte, v uint64) []byte {
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

// legacyNodeID returns the legacy node ID of a typeName object.
func legacyNodeID(typeName string, id int64) string {
	s := "0" + strconv.Itoa(len(typeName)) + ":" + typeName + strconv.FormatInt(id, 10)
	return base64.StdEncoding.EncodeToString([]byte(s))
}

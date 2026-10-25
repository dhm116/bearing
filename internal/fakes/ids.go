package fakes

import (
	"encoding/base64"
	"strconv"
)

// GitHub global node IDs come in two formats. The next format, which
// GitHub returns when a request carries X-Github-Next-Global-ID: 1, is a
// type prefix, "_", and the URL-safe unpadded base64 of a MessagePack
// array: [0, database ID] for users and repositories, [0, org ID, team ID]
// for teams. The legacy format is the standard base64 of
// "0<len(type)>:<type><database ID>". The spec allows only the next format
// in keys.

// legacyNodeID returns the legacy node ID of a typeName object.
func legacyNodeID(typeName string, id int64) string {
	s := "0" + strconv.Itoa(len(typeName)) + ":" + typeName + strconv.FormatInt(id, 10)
	return base64.StdEncoding.EncodeToString([]byte(s))
}

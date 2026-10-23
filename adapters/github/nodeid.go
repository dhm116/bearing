package github

import (
	"fmt"
	"regexp"
	"strings"
)

// GitHub's next-format global node IDs are a type prefix, "_", and the
// URL-safe unpadded base64 of a MessagePack array: [0, database ID] for
// users and repositories, [0, org ID, team ID] for teams (githubid derives
// them). The legacy format is standard base64, which has no "_", so nextID
// tells them apart. Keys hold next-format IDs only.

var nextIDPattern = regexp.MustCompile(`^[A-Z]{1,4}_[A-Za-z0-9_-]+$`)

// nextID returns id if it is a next-format node ID of the given prefix, and
// an error otherwise, so a legacy ID is never emitted as a key.
func nextID(prefix, id string) (string, error) {
	if !nextIDPattern.MatchString(id) || !strings.HasPrefix(id, prefix+"_") {
		return "", fmt.Errorf("%s node ID is not in the next format (is X-Github-Next-Global-ID: 1 honored?)", prefix)
	}
	return id, nil
}

package model

import "regexp"

// SourceManual is the partition of manual events.
const SourceManual = "manual"

// subjectIDPattern is a UUIDv7 in canonical lowercase text.
var subjectIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidSubjectID reports whether id is a canonical UUIDv7.
func ValidSubjectID(id string) bool { return subjectIDPattern.MatchString(id) }

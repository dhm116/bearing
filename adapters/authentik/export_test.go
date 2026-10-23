package authentik

import "testing"

// SetMaxCursorBytes lowers the cursor limit for the test.
func SetMaxCursorBytes(t testing.TB, n int) {
	t.Helper()
	old := maxCursorBytes
	maxCursorBytes = n
	t.Cleanup(func() { maxCursorBytes = old })
}

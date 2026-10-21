package contracts

// Method names an operation the API offers.
type Method string

// The methods of the API in this version.
const (
	// MethodGet is "what is this subject": bearing get.
	MethodGet Method = "query.get"
	// MethodOwner is "who owns this": bearing owner.
	MethodOwner Method = "query.owner"
	// MethodRelated is "what is it related to": bearing related.
	MethodRelated Method = "query.related"
	// MethodChanges is "what changed": bearing changes.
	MethodChanges Method = "query.changes"
	// MethodSyncRequest asks for a sync of one Source, which appends a
	// SyncRequested event.
	MethodSyncRequest Method = "sync.request"
)

// Caller is who is asking, as the authenticator verified it. It never holds
// a token.
type Caller struct {
	// Subject is "token" for the static API token, or "local:<uid>" for the
	// socket's accepted caller.
	Subject string
	// Local is set only by the server for a caller it accepted on the
	// Unix socket (C-API-1).
	Local bool
}

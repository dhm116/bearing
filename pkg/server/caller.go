package server

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
	// MethodFind is "what matches this name": bearing find.
	MethodFind Method = "query.find"
	// MethodLinks is "which identity links are waiting for review": bearing
	// links.
	MethodLinks Method = "links.list"
	// MethodDecide records a person's decision on an identity link, which
	// appends a LinkDecided event.
	MethodDecide Method = "links.decide"
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
	// Agent is set by an authenticator that can tell the caller is an agent
	// (software acting for a person) and not a person. Agents read but do not
	// decide identity links.
	//
	// shortcut: no authenticator sets it yet, so the API token and the
	// socket's UID count as people (docs/spec/api.md, "Link decisions");
	// upgrade when an OIDC authenticator can read an agent claim.
	Agent bool
}

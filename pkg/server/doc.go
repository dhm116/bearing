// Package server is Bearing's always-on core (issue #139, ADR 7). It takes in
// what sources push, authenticates it for the Source it is routed to and puts
// it on the durable event log before acknowledging it; apply workers read the
// log, and the scheduler, retention job and health checks run beside them.
// API listeners build on the same log.
//
// Ingest is the webhook surface (threat model C-INGEST-1 to C-INGEST-11): an
// http.Handler that answers with a status code and a request ID and nothing
// else.
//
// Server is everything behind it (C-INGEST-12). A SyncRequested event makes
// the source's adapter page its output back onto the log as
// ObservationsEmitted events; a WebhookReceived event goes to the adapter's
// Handle the same way; each observation of an ObservationsEmitted event is
// applied through the resolver. A partition is applied in order by one worker,
// an event that keeps failing becomes a dead letter, and a lock on the store
// (store.LockInstance) keeps a second server away (C-STORE-12).
package server

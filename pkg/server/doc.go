// Package server is Bearing's always-on core. It takes in
// what sources push, authenticates it for the Source it is routed to and puts
// it on the durable event log before acknowledging it; apply workers read the
// log, and the scheduler, retention job and health checks run beside them.
// The API serves callers on the same log.
//
// Ingest is the webhook surface (C-INGEST-1 to C-INGEST-11 in
// docs/security/threat-model.md): an http.Handler that answers with a status
// code and a request ID and nothing else.
//
// API is the surface for callers (C-API-1 to C-API-7 in the threat model,
// docs/spec/api.md): the query routes and sync requests, on a Unix socket for
// local callers and a TLS listener for callers with the API token. Every
// route goes through one wrapper that authenticates and rate-limits before it
// reads a parameter.
//
// Server is everything behind it (C-INGEST-12). A SyncRequested event makes
// the source's adapter page its output back onto the log as
// ObservationsEmitted events; a WebhookReceived event goes to the adapter's
// Handle the same way; each observation of an ObservationsEmitted event is
// applied by the Applier, one page at a time. A partition is applied in order by one worker,
// an event that keeps failing becomes a dead letter.
package server

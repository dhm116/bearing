// Package server is Bearing's always-on core (issue #139, ADR 7). It takes in
// what sources push, authenticates it for the Source it is routed to and puts
// it on the durable event log before acknowledging it; the apply workers,
// scheduler and API listeners build on the same log.
//
// Ingest is the webhook surface (threat model C-INGEST-1 to C-INGEST-11): an
// http.Handler that answers with a status code and a request ID and nothing
// else.
package server

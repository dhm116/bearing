// Package testkit holds test fakes for the things Bearing code must not touch
// directly in tests: wall-clock time, randomness in identifiers, the network
// and secrets.
//
//   - [FakeClock] is a controllable [Clock] with timers and tickers.
//   - [SeqIDs] mints deterministic identifiers behind [IDs].
//   - [NewScriptServer] and [NewFixtureServer] wrap httptest servers that
//     serve scripted or recorded responses and record every request.
//   - [Secrets] is a fake [SecretResolver] that hands out canary values, and
//     [FindLeaks] / [AssertNoLeaks] look for those canaries, raw or encoded,
//     in logs, spans, errors and responses.
//
// The interfaces declared here ([Clock], [IDs], [SecretResolver]) document
// what the fakes provide. Production packages do not import testkit: each
// declares its own small interface (often a single method such as
// Now() time.Time) or function field, and these fakes satisfy it. Keep that
// direction, so test helpers never become production dependencies.
package testkit

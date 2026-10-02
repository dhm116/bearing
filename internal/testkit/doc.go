// Package testkit holds test fakes for the things Bearing code must not touch
// directly in tests: wall-clock time, randomness in identifiers, the network
// and secrets.
//
//   - [FakeClock] implements [clock.Clock], with timers and tickers, and only
//     moves when the test moves it.
//   - [SeqIDs] mints deterministic identifiers behind [IDs].
//   - [NewScriptServer] and [NewFixtureServer] wrap httptest servers that
//     serve scripted or recorded responses and record every request.
//   - [Secrets] is a fake [SecretResolver] that hands out canary values, and
//     [FindLeaks] / [AssertNoLeaks] look for those canaries, raw or encoded,
//     in logs, spans, errors and responses.
//
// Production packages never import testkit. Time goes through
// [clock.Clock] in pkg/clock, which FakeClock implements. [IDs] and
// [SecretResolver] are declared here only to document what the fakes
// provide: a production package declares its own small interface with the
// same method (or a function field), and the fake satisfies it.
package testkit

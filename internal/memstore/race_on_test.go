//go:build race

package memstore

// raceSlowdown scales the time bounds of the work-bound tests: the race
// detector makes the same work several times slower.
const raceSlowdown = 5

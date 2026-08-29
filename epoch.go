package cloudrig

import "time"

// fakeEpoch is where every MustStart FakeClock begins. Fixed rather than the
// current time, so timestamps derived from it are stable across runs.
func fakeEpoch() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

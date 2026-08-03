package migrations

import (
	"testing"
	"time"
)

// Compile-time check that SweepStale's signature stays stable. The real
// behaviour is exercised by the testcontainers suite (M18); this is the unit
// surface area we can hit without a live Postgres.
var _ = SweepStale

func TestSweep_DurationCast(t *testing.T) {
	// Belt-and-suspenders: the duration → seconds conversion shouldn't overflow
	// for any time.Duration we'd realistically pass.
	for _, d := range []time.Duration{0, time.Second, time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		secs := int(d.Seconds())
		if d > 0 && secs == 0 {
			t.Fatalf("duration %v rounded to 0 seconds", d)
		}
	}
}

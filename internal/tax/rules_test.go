package tax

import (
	"testing"
	"time"
)

// Holding periods count calendar days in the user's time zone: bought late
// on Dec 31 in New York (Jan 1 in UTC), sold just after midnight on Jan 1
// a year later is long-term there and short-term in UTC.
func TestLongTermFollowsTheUsersCalendar(t *testing.T) {
	bought := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)
	sold := time.Date(2025, 1, 1, 5, 30, 0, 0, time.UTC)
	if (US{}).LongTerm(bought, sold) {
		t.Error("UTC: sold on the anniversary, should be short-term")
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data")
	}
	if !(US{}).LongTerm(bought.In(ny), sold.In(ny)) {
		t.Error("New York: bought Dec 31, sold Jan 1 a year later, should be long-term")
	}
	// The anniversary itself is still short-term; the day after is long.
	a := time.Date(2023, 6, 15, 12, 0, 0, 0, ny)
	if (US{}).LongTerm(a, time.Date(2024, 6, 15, 23, 0, 0, 0, ny)) || !(US{}).LongTerm(a, time.Date(2024, 6, 16, 0, 1, 0, 0, ny)) {
		t.Error("anniversary boundary")
	}
}

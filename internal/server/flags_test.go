package server

import (
	"testing"
	"time"
)

func TestEventFlag(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		late    time.Duration
		chained bool
		want    flag
	}{
		{2 * time.Minute, true, flagNone},
		{-time.Minute, true, flagNone}, // client clock a bit ahead
		{time.Hour, true, flagLate},
		{30 * time.Hour, true, flagStale},
		{5 * 24 * time.Hour, true, flagBackfill},
		{time.Minute, false, flagUnchained},
	}
	for _, c := range cases {
		if got := eventFlag(now.Add(-c.late), now, c.chained); got != c.want {
			t.Errorf("%s late, chained %v: got %q, want %q", c.late, c.chained, got, c.want)
		}
	}
}

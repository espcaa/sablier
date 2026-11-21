package server

import "time"

// how suspicious an event looks to a reviewer, worst last
type flag int

const (
	flagNone      flag = iota
	flagLate           // offline for a bit, synced later the same day
	flagStale          // over a day late
	flagBackfill       // days late, a reviewer has to look at it
	flagUnchained      // from a client before the chain
)

func (f flag) String() string {
	return [...]string{"", "late", "stale", "backfill", "unchained"}[f]
}

// the launcher syncs every 2 minutes while an agent runs, so anything later went through
// offline time or a manual sync
func eventFlag(ts, received time.Time, chained bool) flag {
	if !chained {
		return flagUnchained
	}
	switch d := received.Sub(ts); {
	case d > 72*time.Hour:
		return flagBackfill
	case d > 24*time.Hour:
		return flagStale
	case d > 10*time.Minute:
		return flagLate
	}
	return flagNone
}

package server

import (
	"context"
	"time"

	"github.com/espcaa/sablier/internal/credit"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/jackc/pgx/v5/pgtype"
)

func checkReviewQuota(ctx context.Context, q *sqlc.Queries, user string, install pgtype.UUID, project string, through int64, now time.Time) error {
	rows, err := q.ReviewQuotaEvents(ctx, sqlc.ReviewQuotaEventsParams{
		UserID: user, InstallID: install, ProjectID: project, ThroughSeq: through, UntilTs: pgTime(now),
	})
	if err != nil {
		return err
	}
	return reviewQuota(rows, now)
}

func reviewQuota(rows []sqlc.ReviewQuotaEventsRow, now time.Time) error {
	proposed := make([]event.Event, 0, len(rows))
	previous := make([]event.Event, 0, len(rows))
	for _, row := range rows {
		e := event.Event{
			Seq: row.Seq, TS: row.Ts.Time, Kind: event.Kind(row.Kind),
			SessionID: row.InstallID.String() + ":" + row.SessionID,
			ProjectID: row.ProjectID, Data: row.Data,
		}
		proposed = append(proposed, e)
		if row.AlreadyReviewed {
			previous = append(previous, e)
		}
	}
	if len(proposed) == len(previous) {
		return nil
	}
	totals := reviewMonthlyCredit(proposed, now)
	var baseline map[time.Time]time.Duration
	for month, total := range totals {
		if total <= monthlyReviewLimit {
			continue
		}
		if baseline == nil {
			baseline = reviewMonthlyCredit(previous, now)
		}
		if total > baseline[month] {
			return errReviewBudget
		}
	}
	return nil
}

func reviewMonthlyCredit(events []event.Event, now time.Time) map[time.Time]time.Duration {
	totals := make(map[time.Time]time.Duration)
	credit.InspectBetween(events, credit.Default, time.Time{}, now, func(i credit.Interval) {
		for start := i.Start.UTC(); start.Before(i.End); {
			month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
			end := month.AddDate(0, 1, 0)
			if i.End.Before(end) {
				end = i.End
			}
			totals[month] += end.Sub(start)
			start = end
		}
	})
	return totals
}

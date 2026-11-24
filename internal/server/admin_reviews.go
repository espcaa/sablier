package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/espcaa/sablier/internal/credit"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/views"
	"github.com/jackc/pgx/v5/pgtype"
)

var errNoReview = errors.New("project has no analyzed submission")

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func (s *Server) adminReviews(w http.ResponseWriter, r *http.Request) {
	user := s.requireAdmin(w, r)
	if user == nil {
		return
	}
	me := views.Me{Name: user.Name, SlackID: user.SlackID.String}
	q := r.URL.Query()
	if q.Get("account") == "" && q.Get("project") == "" {
		rows, err := s.q.ReviewProjects(r.Context())
		if err != nil {
			reviewError(w, err)
			return
		}
		views.ReviewIndex(me, rows).Render(r.Context(), w)
		return
	}
	account, project := q.Get("account"), q.Get("project")
	if account == "" || !validProjectID.MatchString(project) {
		http.Error(w, "select an account and project", http.StatusBadRequest)
		return
	}
	report, err := s.projectReview(r.Context(), account, project, time.Now())
	if err != nil {
		reviewError(w, err)
		return
	}
	views.ProjectReview(me, report).Render(r.Context(), w)
}

func reviewError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoReview) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("load project review: %v", err)
	http.Error(w, "could not load project review", http.StatusInternalServerError)
}

type reviewFocus struct {
	at time.Time
	in bool
}

func focusedTime(changes []reviewFocus, start, end time.Time) time.Duration {
	i := sort.Search(len(changes), func(i int) bool { return changes[i].at.After(start) })
	var result time.Duration
	in := i > 0 && changes[i-1].in
	for ; i < len(changes) && changes[i].at.Before(end); i++ {
		if in {
			result += changes[i].at.Sub(start)
		}
		start, in = changes[i].at, changes[i].in
	}
	if in {
		result += end.Sub(start)
	}
	return result
}

func (s *Server) projectReview(ctx context.Context, account, project string, now time.Time) (views.ProjectReviewData, error) {
	report := views.ProjectReviewData{
		Account: account, Project: project,
	}
	scopes, err := s.q.ReviewProjectScopes(ctx, sqlc.ReviewProjectScopesParams{UserID: account, ProjectID: project})
	if err != nil {
		return report, err
	}
	if len(scopes) == 0 {
		return report, errNoReview
	}
	cutoffs := make(map[string]int64, len(scopes))
	for _, row := range scopes {
		cutoffs[row.InstallID.String()] = row.LastSeq
		report.Unavailable += row.Unavailable
	}
	report.Installs = len(cutoffs)
	start := time.Time{}
	rows, err := s.q.UserEventsBetween(ctx, sqlc.UserEventsBetweenParams{
		UserID: account, FromTs: pgTime(start), ToTs: pgTime(now),
	})
	if err != nil {
		return report, err
	}
	labels, err := s.q.ProjectReviewSignals(ctx, sqlc.ProjectReviewSignalsParams{
		UserID: account, ProjectID: project, FromTs: pgTime(start), ToTs: pgTime(now),
	})
	if err != nil {
		return report, err
	}
	report.Jev = make(map[string]int)
	for _, row := range labels {
		if row.Seq <= cutoffs[row.InstallID.String()] {
			report.Jev[row.Label]++
			report.Classified++
		}
	}
	var events, allEvents []event.Event
	focus := map[string][]reviewFocus{}
	counts := make(map[time.Time]int)

	for _, row := range rows {
		install := row.InstallID.String()
		isProject := row.ProjectID == project
		e := event.Event{Seq: row.Seq, TS: row.Ts.Time, Kind: event.Kind(row.Kind), SessionID: row.SessionID, ProjectID: row.ProjectID, Data: row.Data}
		allEvents = append(allEvents, e)
		if isProject && e.Kind == event.PromptSent {
			report.TotalPrompts++
		}
		if isProject && row.Seq > cutoffs[install] {
			continue
		}
		session := install + ":" + row.SessionID
		e.SessionID = session
		events = append(events, e)
		if !isProject {
			continue
		}
		if e.Kind == event.Focus {
			if payload, err := e.Payload(); err == nil {
				focus[session] = append(focus[session], reviewFocus{e.TS, payload.(*event.FocusData).In})
			}
		}
		report.EventCount++
		counts[e.TS.UTC().Truncate(time.Hour)]++
		delay := row.ReceivedAt.Time.Sub(e.TS)
		if delay < 0 {
			report.Early++
		}
		switch timingFlag(delay) {
		case flagLate:
			report.Late++
		case flagStale, flagBackfill:
			report.Offline++
		}
		if !row.Hash.Valid || row.Hash.String == "" {
			report.Unchained++
		}
		if e.Kind == event.PromptSent {
			report.Prompts++
		}
	}
	rep := credit.InspectBetween(events, credit.Default, start, now, func(interval credit.Interval) {
		if interval.Project != project {
			return
		}
		duration := interval.End.Sub(interval.Start)

		if interval.ObservedInput {
			report.Observed += duration
		}
		if interval.MouseInput {
			report.Mouse += duration
		}
		report.Focused += focusedTime(focus[interval.Session], interval.Start, interval.End)
	})
	report.Credited = rep.ByProject[project]
	report.TotalTracked = credit.Compute(allEvents, credit.Default, now).ByProject[project]
	report.Heatmap = buildReviewHeatmap(counts)
	return report, nil
}

func buildReviewHeatmap(counts map[time.Time]int) []views.ReviewHeatDay {
	activeDays := map[time.Time]bool{}
	for hour := range counts {
		activeDays[midnight(hour)] = true
	}
	dates := make([]time.Time, 0, len(activeDays))
	for day := range activeDays {
		dates = append(dates, day)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	days := make([]views.ReviewHeatDay, 0, len(dates))
	for _, day := range dates {
		row := views.ReviewHeatDay{Label: day.Format("02 Jan 2006")}
		for hour := range row.Hours {
			at := day.Add(time.Duration(hour) * time.Hour)
			count := counts[at]
			row.Hours[hour] = views.ReviewHeatCell{Count: count, Label: fmt.Sprintf("%s UTC · %d heartbeats", at.Format("02 Jan 2006 15:00"), count)}
		}
		days = append(days, row)
	}
	return days
}

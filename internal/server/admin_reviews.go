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
	account, project := r.URL.Query().Get("account"), r.URL.Query().Get("project")
	if account == "" && project == "" {
		rows, err := s.q.ReviewProjects(r.Context())
		if err != nil {
			reviewError(w, err)
			return
		}
		views.ReviewIndex(me, rows).Render(r.Context(), w)
		return
	}
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

type focusChange struct {
	at time.Time
	in bool
}

func focusedTime(changes []focusChange, start, end time.Time) time.Duration {
	i := sort.Search(len(changes), func(i int) bool { return changes[i].at.After(start) })
	in := i > 0 && changes[i-1].in
	var total time.Duration
	for ; i < len(changes) && changes[i].at.Before(end); i++ {
		if in {
			total += changes[i].at.Sub(start)
		}
		start, in = changes[i].at, changes[i].in
	}
	if in {
		total += end.Sub(start)
	}
	return total
}

func (s *Server) projectReview(ctx context.Context, account, project string, now time.Time) (views.ProjectReviewData, error) {
	report := views.ProjectReviewData{Account: account, Project: project, Labels: map[string]int{}}
	scopes, err := s.q.ReviewProjectScopes(ctx, sqlc.ReviewProjectScopesParams{UserID: account, ProjectID: project})
	if err != nil {
		return report, err
	}
	if len(scopes) == 0 {
		return report, errNoReview
	}
	cutoffs := map[string]int64{}
	for _, row := range scopes {
		cutoffs[row.InstallID.String()] = row.LastSeq
		report.AccountName = row.Name
	}
	report.Installs = len(cutoffs)
	labels, err := s.q.ReviewLabelCounts(ctx, sqlc.ReviewLabelCountsParams{UserID: account, ProjectID: project})
	if err != nil {
		return report, err
	}
	for _, row := range labels {
		report.Labels[row.Label] = int(row.N)
	}
	rows, err := s.q.UserEventsBetween(ctx, sqlc.UserEventsBetweenParams{UserID: account, FromTs: pgTime(time.Time{}), ToTs: pgTime(now)})
	if err != nil {
		return report, err
	}

	// all events count toward total time, only events up to each install's checkpoint toward submitted time
	var all, submitted []event.Event
	focus := map[string][]focusChange{}
	counts := map[time.Time]int{}
	for _, row := range rows {
		install := row.InstallID.String()
		e := event.Event{Seq: row.Seq, TS: row.Ts.Time, Kind: event.Kind(row.Kind), SessionID: install + ":" + row.SessionID, ProjectID: row.ProjectID, Data: row.Data}
		all = append(all, e)
		isProject := row.ProjectID == project
		if isProject && e.Kind == event.PromptSent {
			report.TotalPrompts++
		}
		if isProject && row.Seq > cutoffs[install] {
			continue
		}
		submitted = append(submitted, e)
		if !isProject {
			continue
		}
		if e.Kind == event.PromptSent {
			report.Prompts++
		}
		if e.Kind == event.Focus {
			if payload, err := e.Payload(); err == nil {
				focus[e.SessionID] = append(focus[e.SessionID], focusChange{e.TS, payload.(*event.FocusData).In})
			}
		}
		report.EventCount++
		counts[e.TS.UTC().Truncate(time.Hour)]++
		delay := row.ReceivedAt.Time.Sub(e.TS)
		switch timingFlag(delay) {
		case flagLate:
			report.Late++
		case flagStale, flagBackfill:
			report.Offline++
		}
		if delay < 0 {
			report.Early++
		}
		if !row.Hash.Valid || row.Hash.String == "" {
			report.Unchained++
		}
	}
	rep := credit.InspectBetween(submitted, credit.Default, time.Time{}, now, func(i credit.Interval) {
		if i.Project != project {
			return
		}
		d := i.End.Sub(i.Start)
		if i.ObservedInput {
			report.Observed += d
		}
		if i.MouseInput {
			report.Mouse += d
		}
		report.Focused += focusedTime(focus[i.Session], i.Start, i.End)
	})
	report.Submitted = rep.ByProject[project]
	report.TotalTracked = credit.Compute(all, credit.Default, now).ByProject[project]
	report.Heatmap = reviewHeatmap(counts)
	return report, nil
}

func reviewHeatmap(counts map[time.Time]int) []views.ReviewHeatDay {
	seen := map[time.Time]bool{}
	var dates []time.Time
	for hour := range counts {
		if day := midnight(hour); !seen[day] {
			seen[day] = true
			dates = append(dates, day)
		}
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	days := make([]views.ReviewHeatDay, len(dates))
	for i, day := range dates {
		days[i].Label = day.Format("02 Jan 2006")
		for h := range days[i].Hours {
			at := day.Add(time.Duration(h) * time.Hour)
			days[i].Hours[h] = views.ReviewHeatCell{Count: counts[at], Label: fmt.Sprintf("%s UTC · %d events", at.Format("02 Jan 2006 15:00"), counts[at])}
		}
	}
	return days
}

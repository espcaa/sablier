package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/espcaa/sablier/internal/credit"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/views"
	"github.com/jackc/pgx/v5/pgtype"
)

// the browser sets the tz cookie
func viewerLocation(r *http.Request) *time.Location {
	if c, err := r.Cookie("tz"); err == nil {
		if loc, err := time.LoadLocation(c.Value); err == nil {
			return loc
		}
	}
	return time.Local
}

func midnight(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// credited time from from to now, zero from means all time.
// the same folder on two machines has two ids but one name, so it's summed
func (s *Server) summary(ctx context.Context, userID string, from, now time.Time) (views.Summary, error) {
	rows, err := s.q.UserEventsBetween(ctx, sqlc.UserEventsBetweenParams{
		UserID: userID,
		FromTs: pgtype.Timestamptz{Time: from, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return views.Summary{}, err
	}
	events := make([]event.Event, len(rows))
	harness := map[string]string{}
	for i, e := range rows {
		events[i] = event.Event{
			Seq:       e.Seq,
			TS:        e.Ts.Time,
			Kind:      event.Kind(e.Kind),
			SessionID: e.SessionID,
			ProjectID: e.ProjectID,
			Harness:   e.Harness,
			Source:    event.Source(e.Source),
			Data:      e.Data,
		}
		if e.Harness != "" {
			harness[e.SessionID] = e.Harness
		}
	}
	rep := credit.Compute(events, credit.Default, now)

	projects, err := s.q.UserProjects(ctx, userID)
	if err != nil {
		return views.Summary{}, err
	}
	names := map[string]string{}
	for _, p := range projects {
		names[p.ProjectID] = p.Name
	}
	byName := map[string]time.Duration{}
	for id, d := range rep.ByProject {
		name := names[id]
		if name == "" {
			name = "(unnamed)"
		}
		byName[name] += d
	}

	var sessions []string
	for id := range rep.BySession {
		sessions = append(sessions, id)
	}
	models, err := s.q.SessionModels(ctx, sqlc.SessionModelsParams{UserID: userID, Sessions: sessions})
	if err != nil {
		return views.Summary{}, err
	}
	model := map[string]string{}
	for _, m := range models {
		model[m.SessionID] = m.Model
	}

	byHarness := map[string]time.Duration{}
	byModel := map[string]time.Duration{}
	for id, d := range rep.BySession {
		if h := harness[id]; h != "" {
			byHarness[h] += d
		}
		if m := model[id]; m != "" {
			byModel[m] += d
		}
	}

	return views.Summary{
		Total:     rep.Credited,
		Projects:  ranked(byName),
		Harnesses: ranked(byHarness),
		Models:    ranked(byModel),
	}, nil
}

// most time first, ties by name
func ranked(m map[string]time.Duration) []views.Entry {
	list := make([]views.Entry, 0, len(m))
	for name, d := range m {
		if d > 0 {
			list = append(list, views.Entry{Name: name, Time: d})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Time != list[j].Time {
			return list[i].Time > list[j].Time
		}
		return list[i].Name < list[j].Name
	})
	return list
}

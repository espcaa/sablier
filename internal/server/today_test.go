package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/jackc/pgx/v5/pgtype"
)

type seed struct {
	q   *sqlc.Queries
	seq map[string]int64
}

func (sd *seed) add(t *testing.T, install string, kind event.Kind, at time.Time, project string, data any) {
	t.Helper()
	var id pgtype.UUID
	id.Scan(install)
	sd.q.ClaimInstall(context.Background(), sqlc.ClaimInstallParams{ID: id, UserID: "ident!a"})
	b, _ := json.Marshal(data)
	sd.seq[install]++
	_, err := sd.q.InsertEvent(context.Background(), sqlc.InsertEventParams{
		InstallID: id, Seq: sd.seq[install], Ts: pgtype.Timestamptz{Time: at, Valid: true},
		Kind: string(kind), SessionID: "run:" + install, ProjectID: project,
		Harness: "claude-code", Source: "adapter", Data: b,
	})
	if err != nil {
		t.Fatal(err)
	}
}

const installB = "22222222-2222-4222-8222-222222222222"

func TestToday(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: "ident!a", ApiKey: "sab_a"})
	s := &Server{q: q}
	sd := &seed{q: q, seq: map[string]int64{}}
	noon := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)

	// laptop: 6m on sablier (1m reading back to session start + 5m after the prompt)
	sd.add(t, installA, event.SessionStart, noon, "pa1", event.SessionStartData{})
	sd.add(t, installA, event.PromptSent, noon.Add(time.Minute), "pa1", event.PromptSentData{})
	// desktop: the same folder, different id, 5m later in the day
	sd.add(t, installB, event.PromptSent, noon.Add(time.Hour), "pb1", event.PromptSentData{})
	// yesterday doesn't count, but its session's model does
	sd.add(t, installB, event.SessionStart, noon.Add(-24*time.Hour-time.Minute), "pb1", event.SessionStartData{Model: "claude-opus-5-5"})
	sd.add(t, installB, event.PromptSent, noon.Add(-24*time.Hour), "pb1", event.PromptSentData{})

	q.UpsertProject(ctx, sqlc.UpsertProjectParams{InstallID: uuidOf(installA), ProjectID: "pa1", Name: "sablier"})
	q.UpsertProject(ctx, sqlc.UpsertProjectParams{InstallID: uuidOf(installB), ProjectID: "pb1", Name: "sablier"})

	now := noon.Add(3 * time.Hour)
	sum, err := s.summary(ctx, "ident!a", midnight(now), now)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 11*time.Minute || len(sum.Projects) != 1 || sum.Projects[0].Name != "sablier" || sum.Projects[0].Time != 11*time.Minute {
		t.Fatalf("got %+v", sum)
	}
	if len(sum.Harnesses) != 1 || sum.Harnesses[0].Name != "claude-code" || sum.Harnesses[0].Time != 11*time.Minute {
		t.Fatalf("harnesses %+v", sum.Harnesses)
	}
	if len(sum.Models) != 1 || sum.Models[0].Name != "claude-opus-5-5" || sum.Models[0].Time != 5*time.Minute {
		t.Fatalf("models %+v", sum.Models)
	}

	// all time adds yesterday's 1m+5m, and the desktop prompt now earns 5m before it too
	all, err := s.summary(ctx, "ident!a", time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 22*time.Minute || all.Projects[0].Time != 22*time.Minute {
		t.Fatalf("all time %+v", all)
	}
}

func uuidOf(s string) pgtype.UUID {
	var id pgtype.UUID
	id.Scan(s)
	return id
}

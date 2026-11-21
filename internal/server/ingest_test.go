package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
)

const installA = "11111111-1111-4111-8111-111111111111"

func post(h http.Handler, key string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/events", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ev(seq int64) event.Event {
	e, _ := event.New(event.PromptSent, time.Date(2026, 11, 20, 10, 0, int(seq), 0, time.UTC), event.PromptSentData{Hash: "h"})
	e.Seq, e.SessionID, e.ProjectID, e.Harness, e.Source = seq, "run:x", "p1", "claude-code", event.SourceAdapter
	return e
}

func ingestSetup(t *testing.T) (http.Handler, *sqlc.Queries) {
	pool := testPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: "ident!a", ApiKey: "sab_a"})
	q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: "ident!b", ApiKey: "sab_b"})
	return New(Config{}, pool, q, nil), q
}

func TestIngest(t *testing.T) {
	h, q := ingestSetup(t)
	batch := api.Batch{
		InstallID: installA,
		Events:    []event.Event{ev(1), ev(2)},
		Projects:  []api.Project{{ID: "p1", Name: "sablier"}},
	}

	rec := post(h, "sab_a", batch)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accepted":2,"duplicates":0`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	// a retry with one new event
	batch.Events = append(batch.Events, ev(3))
	rec = post(h, "sab_a", batch)
	if !strings.Contains(rec.Body.String(), `"accepted":1,"duplicates":2`) {
		t.Fatalf("retry: %s", rec.Body)
	}

	ps, _ := q.UserProjects(context.Background(), "ident!a")
	if len(ps) != 1 || ps[0].Name != "sablier" {
		t.Fatalf("projects %+v", ps)
	}
}

func TestIngestOtherAccountsInstall(t *testing.T) {
	h, _ := ingestSetup(t)
	post(h, "sab_a", api.Batch{InstallID: installA, Events: []event.Event{ev(1)}})

	rec := post(h, "sab_b", api.Batch{InstallID: installA, Events: []event.Event{ev(2)}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestIngestRejects(t *testing.T) {
	h, _ := ingestSetup(t)
	unknown := ev(1)
	unknown.Kind = "made_up"
	noSeq := ev(1)
	noSeq.Seq = 0
	tooMany := make([]event.Event, api.MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = ev(int64(i + 1))
	}

	cases := map[string]api.Batch{
		"bad install":        {InstallID: "nope", Events: []event.Event{ev(1)}},
		"unknown kind":       {InstallID: installA, Events: []event.Event{unknown}},
		"no seq":             {InstallID: installA, Events: []event.Event{noSeq}},
		"too many":           {InstallID: installA, Events: tooMany},
		"control char in id": {InstallID: installA, Events: []event.Event{ev(1)}, Projects: []api.Project{{ID: "a\nb", Name: "ab"}}},
		"path as name":       {InstallID: installA, Events: []event.Event{ev(1)}, Projects: []api.Project{{ID: "p1", Name: "/Users/alice/sablier"}}},
	}
	for name, b := range cases {
		if rec := post(h, "sab_a", b); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}

	// nothing from a rejected batch is stored, not even the install
	rec := post(h, "sab_b", api.Batch{InstallID: installA, Events: []event.Event{ev(1)}})
	if rec.Code != 200 {
		t.Fatalf("install was claimed by a rejected batch: %d %s", rec.Code, rec.Body)
	}
}

// links events like the client does, continuing from prev
func chain(prev string, es ...event.Event) []event.Event {
	for i := range es {
		es[i].PrevHash = prev
		es[i].Hash, _ = event.ChainHash(prev, es[i])
		prev = es[i].Hash
	}
	return es
}

func TestIngestChain(t *testing.T) {
	h, _ := ingestSetup(t)

	// a client from before the chain, then the same install after updating
	if rec := post(h, "sab_a", api.Batch{InstallID: installA, Events: []event.Event{ev(1), ev(2)}}); rec.Code != 200 {
		t.Fatalf("unchained: %d %s", rec.Code, rec.Body)
	}
	sent := chain("", ev(3), ev(4), ev(5))
	if rec := post(h, "sab_a", api.Batch{InstallID: installA, Events: sent}); !strings.Contains(rec.Body.String(), `"accepted":3,"duplicates":0`) {
		t.Fatalf("chain start: %d %s", rec.Code, rec.Body)
	}

	// sync --all sends everything again
	all := append([]event.Event{ev(1), ev(2)}, sent...)
	if rec := post(h, "sab_a", api.Batch{InstallID: installA, Events: all}); !strings.Contains(rec.Body.String(), `"accepted":0,"duplicates":5`) {
		t.Fatalf("resend: %d %s", rec.Code, rec.Body)
	}

	head := sent[2].Hash
	edited := sent[1]
	edited.Data = []byte(`{"hash":"other"}`)
	rehashed := chain(sent[0].Hash, edited)[0]
	future := ev(6)
	future.TS = time.Now().Add(time.Hour)

	attacks := map[string]struct {
		events []event.Event
		code   int
	}{
		"edited, hash kept":        {[]event.Event{edited}, http.StatusBadRequest},
		"edited and rehashed":      {[]event.Event{rehashed}, http.StatusConflict},
		"skips an event":           {chain(head, ev(7)), http.StatusConflict},
		"links to an older event":  {chain(sent[1].Hash, ev(6)), http.StatusConflict},
		"starts a new chain":       {chain("", ev(6)), http.StatusConflict},
		"inserted unchained first": {append([]event.Event{ev(6)}, chain(head, ev(7))...), http.StatusConflict},
		"in the future":            {chain(head, future), http.StatusBadRequest},
		"unchained after chain":    {[]event.Event{ev(6)}, http.StatusConflict},
	}
	for name, a := range attacks {
		if rec := post(h, "sab_a", api.Batch{InstallID: installA, Events: a.events}); rec.Code != a.code {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}

	// none of that moved the chain, the real next event still fits
	if rec := post(h, "sab_a", api.Batch{InstallID: installA, Events: chain(head, ev(6))}); rec.Code != 200 {
		t.Fatalf("after attacks: %d %s", rec.Code, rec.Body)
	}
}

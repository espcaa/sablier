package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPrivateProjectReview(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	seedDevice(t, q, "member", "Member", "member-key")
	seedDevice(t, q, "admin", "Admin", "admin-key")
	for _, id := range []string{"admin", "member"} {
		if err := q.CreateSession(t.Context(), sqlc.CreateSessionParams{
			TokenHash: hash(id), UserID: id, ExpiresAt: pgTime(time.Now().Add(time.Hour)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(Config{AdminUserIDs: []string{"admin"}}, pool, q, nil)
	at := time.Now().UTC().Add(-60 * 24 * time.Hour)
	events := []event.Event{
		ev(1), ev(2), ev(3), ev(4),
	}
	events[0].Kind, events[0].Data = event.Focus, json.RawMessage(`{"in":true}`)
	events[1].Kind, events[1].Data = event.InputTick, json.RawMessage(`{"window_ms":10000,"mouse":1}`)
	for i := range events {
		events[i].TS = at.Add(time.Duration(i) * time.Minute)
	}
	if res := post(handler, "member-key", api.Batch{InstallID: installA, Events: events}); res.Code != http.StatusOK {
		t.Fatal(res.Body)
	}
	id := pgtype.UUID{Bytes: uuid.MustParse(installA), Valid: true}
	if err := q.AdvanceAnalysis(t.Context(), sqlc.AdvanceAnalysisParams{UserID: "member", InstallID: id, ProjectID: "p1", LastSeq: 3}); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveReviewSignal(t.Context(), sqlc.SaveReviewSignalParams{
		UserID: "member", InstallID: id, ProjectID: "p1", Seq: 3, EventHour: reviewHour(at), Label: "iteration", Model: "jev-test",
		Metadata: []byte(`{"shell_calls":2}`),
	}); err != nil {
		t.Fatal(err)
	}
	scope := url.Values{"account": {"member"}, "project": {"p1"}}
	call := func(method, path, session string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(form.Encode()))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		if method == http.MethodPost {
			req.Header.Set("Origin", "http://example.com")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if res := call(http.MethodGet, "/admin/reviews", "member", nil); res.Code != http.StatusForbidden {
		t.Fatal(res.Code)
	}
	if res := call(http.MethodGet, "/admin/reviews", "admin", nil); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Member") {
		t.Fatal("missing review index")
	}
	path := "/admin/reviews?" + scope.Encode()
	res := call(http.MethodGet, path, "admin", nil)
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "Heartbeat heatmap") || !strings.Contains(body, "Backlog (over 24h)") ||
		!strings.Contains(body, at.Format("02 Jan 2006")) || !strings.Contains(body, "Verification coverage") || !strings.Contains(body, "1 of 2 tracked prompts") || !strings.Contains(body, "Total tracked time") || strings.Contains(body, "From UTC") || strings.Contains(body, "Heartbeat timeline") ||
		strings.Contains(body, `<a class="heatmap-cell"`) {
		t.Fatalf("unexpected project evidence (%d)", res.Code)
	}
	if res := call(http.MethodPost, "/admin/reviews", "admin", scope); res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected read-only evidence page, got %d", res.Code)
	}
}

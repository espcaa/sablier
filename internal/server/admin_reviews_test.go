package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAdminProjectReview(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	for _, id := range []string{"admin", "member"} {
		seedDevice(t, q, id, strings.ToUpper(id[:1])+id[1:], id+"-key")
		if err := q.CreateSession(t.Context(), sqlc.CreateSessionParams{TokenHash: hash(id), UserID: id, ExpiresAt: pgTime(time.Now().Add(time.Hour))}); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(Config{AdminUserIDs: []string{"admin"}}, pool, q, nil)
	if res := post(handler, "member-key", api.Batch{InstallID: installA, Events: []event.Event{ev(1), ev(2)}}); res.Code != http.StatusOK {
		t.Fatal(res.Body)
	}
	id := pgtype.UUID{Bytes: uuid.MustParse(installA), Valid: true}
	if err := q.AdvanceAnalysis(t.Context(), sqlc.AdvanceAnalysisParams{UserID: "member", InstallID: id, ProjectID: "p1", LastSeq: 1}); err != nil {
		t.Fatal(err)
	}
	get := func(path, session string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/admin/reviews", "member"); code != http.StatusForbidden {
		t.Fatalf("member got %d", code)
	}
	if code, body := get("/admin/reviews", "admin"); code != http.StatusOK || !strings.Contains(body, "Member") {
		t.Fatalf("index: %d", code)
	}
	if code, body := get("/admin/reviews?account=member&project=p1", "admin"); code != http.StatusOK || !strings.Contains(body, "1 / 2") {
		t.Fatalf("project review: %d", code)
	}
}

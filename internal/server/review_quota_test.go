package server

import (
	"errors"
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

func TestReviewQuotaCountsTimeOnceAcrossProjectsAndMonths(t *testing.T) {
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feb := jan.AddDate(0, 1, 0)
	now := feb.AddDate(0, 0, 2)
	tick := func(id uuid.UUID, project string, at time.Time, reviewed bool) sqlc.ReviewQuotaEventsRow {
		return sqlc.ReviewQuotaEventsRow{
			InstallID: pgtype.UUID{Bytes: id, Valid: true}, Ts: pgTime(at), Kind: "input_tick",
			SessionID: "same-session-name", ProjectID: project, Data: []byte(`{"window_ms":3600000,"keys":1}`), AlreadyReviewed: reviewed,
		}
	}
	a, b := uuid.New(), uuid.New()
	var reviewed []sqlc.ReviewQuotaEventsRow
	for h := 1; h <= 299; h++ {
		reviewed = append(reviewed, tick(a, "first", jan.Add(time.Duration(h)*time.Hour), true))
	}
	if err := reviewQuota(append(reviewed, tick(b, "other", jan.Add(time.Hour), false)), now); err != nil {
		t.Fatalf("overlapping time should count once: %v", err)
	}
	if err := reviewQuota(append(reviewed, tick(b, "other", feb.Add(time.Hour), false)), now); err != nil {
		t.Fatalf("the next UTC month should have its own allowance: %v", err)
	}
	if err := reviewQuota(append(reviewed, tick(b, "other", jan.Add(301*time.Hour), false)), now); !errors.Is(err, errReviewBudget) {
		t.Fatalf("expected account-wide cap, got %v", err)
	}
	if err := reviewQuota(reviewed, now); err != nil {
		t.Fatalf("already reviewed events should not charge again: %v", err)
	}
}

func TestReviewPlanRejectsOverLimitBeforeRequestingText(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	seedDevice(t, q, "test", "", "test-token")
	handler := New(Config{DecisionAPIKey: "not-used"}, pool, q, nil)
	current := time.Now().UTC()
	month := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	batch := api.Batch{InstallID: installA}
	var previous string
	for i := 1; i <= 301; i++ {
		e, err := event.New(event.InputTick, month.Add(time.Duration(i)*time.Hour), event.InputTickData{WindowMs: 3600000, Keys: 1})
		if err != nil {
			t.Fatal(err)
		}
		e.Seq, e.ProjectID, e.SessionID, e.PrevHash = int64(i), "month", "test", previous
		e.Hash, err = event.ChainHash(previous, e)
		if err != nil {
			t.Fatal(err)
		}
		previous = e.Hash
		batch.Events = append(batch.Events, e)
	}
	e, err := event.New(event.PromptSent, month.Add(302*time.Hour), event.PromptSentData{})
	if err != nil {
		t.Fatal(err)
	}
	e.Seq, e.ProjectID, e.SessionID, e.PrevHash = 302, "month", "test", previous
	e.Hash, err = event.ChainHash(previous, e)
	if err != nil {
		t.Fatal(err)
	}
	batch.Events = append(batch.Events, e)
	if rec := post(handler, "test-token", batch); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/plan?project=month&install="+installA, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "300-hour") {
		t.Fatalf("expected quota rejection, got %d %s", rec.Code, rec.Body)
	}
}

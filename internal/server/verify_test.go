package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/cli"
	"github.com/espcaa/sablier/internal/config"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestVerifyResumesAfterLostAcknowledgment(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	seedDevice(t, q, "account-a", "", "sab_a")
	seedDevice(t, q, "account-b", "", "sab_b")
	var models atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-test",
			"answers": map[string]any{"1": map[string]string{"type": "choice", "choice": "iteration"}},
		})
	}))
	defer model.Close()
	handler := New(Config{DecisionAPIKey: "fixture", DecisionAPIURL: model.URL}, pool, q, nil)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/verify" && posts.Add(1) == 1 {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusOK {
				t.Errorf("batch rejected: %d", rec.Code)
			}
			http.Error(w, "lost acknowledgment", 503)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	t.Setenv("SABLIER_HOME", dir)
	t.Setenv("SABLIER_CONFIG", filepath.Join(dir, "config"))
	if err := config.Save(config.Config{APIURL: server.URL, APIKey: "sab_a"}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "sablier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	install, err := s.InstallID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := func(answer, want string, fail bool) {
		t.Helper()
		cmd := cli.NewRootCmd()
		var out bytes.Buffer
		cmd.SetArgs([]string{"verify", "demo"})
		cmd.SetIn(strings.NewReader(answer))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		err := cmd.Execute()
		if (err != nil) != fail || !strings.Contains(out.String(), want) {
			t.Fatalf("verify: %q, %v", out.String(), err)
		}
	}
	run("", "no local prompt data", false)
	salt, err := s.Salt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	key := prompt.Key(salt, "session", at)
	e, err := event.New(event.PromptSent, at, event.PromptSentData{Hash: prompt.Hash(key, "synthetic")})
	if err != nil {
		t.Fatal(err)
	}
	e.ProjectID, e.Prompt, e.SessionID = "demo", "synthetic", "session"
	if _, err := s.Append(t.Context(), e); err != nil {
		t.Fatal(err)
	}

	run("no\n", "cancelled", false)
	if posts.Load() != 0 || models.Load() != 0 {
		t.Fatal("sent prompts before consent")
	}
	run("yes\n", "sending prompts", true)
	run("yes\n", "0 prompts classified", false)
	if posts.Load() != 1 || models.Load() != 1 {
		t.Fatalf("retry submitted evidence again: %d uploads, %d model calls", posts.Load(), models.Load())
	}
	installs, err := api.New(server.URL, "sab_b").ReviewInstalls(t.Context(), "demo")
	if err != nil || len(installs) != 0 {
		t.Fatalf("second account can see another account's install: %v", err)
	}
	seq, err := q.AnalysisCheckpoint(t.Context(), sqlc.AnalysisCheckpointParams{
		UserID: "account-a", InstallID: pgtype.UUID{Bytes: uuid.MustParse(install), Valid: true}, ProjectID: "demo",
	})
	if err != nil || seq != 1 {
		t.Fatalf("missing checkpoint: %d, %v", seq, err)
	}
}

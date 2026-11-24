package server

import (
	"bytes"
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
	"github.com/espcaa/sablier/internal/store"
)

func TestVerifyResumesAfterLostAcknowledgment(t *testing.T) {
	handler, _ := ingestSetup(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && posts.Add(1) == 1 {
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
	for i := 0; i <= api.VerifyBatchSize; i++ {
		e, _ := event.New(event.PromptSent, time.Now().Add(-time.Duration(i)*time.Hour), event.PromptSentData{})
		e.ProjectID, e.Prompt = "demo", "synthetic"
		if _, err := s.Append(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	run("no\n", "cancelled", false)
	if posts.Load() != 0 {
		t.Fatal("sent prompts before consent")
	}
	run("yes\n", "sending prompts", true)
	run("yes\n", "1 unsent prompt • 101 total prompts", false)
	run("", "already up to date", false)
	if posts.Load() != 2 {
		t.Fatalf("got %d uploads", posts.Load())
	}
	cp, err := api.New(server.URL, "sab_b").Checkpoint(t.Context(), "demo", install)
	if err != nil || cp.LastSeq != 0 {
		t.Fatalf("unexpected checkpoint for second account: %+v, %v", cp, err)
	}
	cp, err = api.New(server.URL, "sab_a").Checkpoint(t.Context(), "demo", install)
	if err != nil || cp.LastSeq != api.VerifyBatchSize+1 || cp.ReceivedAt == nil {
		t.Fatalf("missing receipt: %+v, %v", cp, err)
	}
}

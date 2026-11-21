package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/store"
)

type fakeServer struct {
	mu      sync.Mutex
	batches []api.Batch
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sab_good" {
		w.WriteHeader(401)
		return
	}
	var b api.Batch
	json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	f.batches = append(f.batches, b)
	f.mu.Unlock()
	json.NewEncoder(w).Encode(api.Result{Accepted: len(b.Events)})
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "sablier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	n := api.MaxBatch + 3 // two batches
	for i := range n {
		e, _ := event.New(event.InputTick, time.Now().Add(time.Duration(i)*time.Second), event.InputTickData{Keys: 1})
		e.ProjectID = "github.com/espcaa/sablier"
		if _, err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	fake := &fakeServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	client := api.New(srv.URL, "sab_good")

	var out bytes.Buffer
	if err := syncEvents(ctx, s, client, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "synced 503 events") {
		t.Fatalf("got %q", out.String())
	}
	if len(fake.batches) != 2 || len(fake.batches[0].Events) != api.MaxBatch || len(fake.batches[1].Events) != 3 {
		t.Fatalf("batches: %d", len(fake.batches))
	}
	b := fake.batches[0]
	if len(b.InstallID) != 36 || len(b.Projects) != 1 || b.Projects[0].Name != "sablier" {
		t.Fatalf("first batch: install %q, projects %+v", b.InstallID, b.Projects)
	}
	if fake.batches[1].InstallID != b.InstallID || len(fake.batches[1].Projects) != 1 {
		t.Fatal("second batch should keep the install and name its projects")
	}

	out.Reset()
	syncEvents(ctx, s, client, &out)
	if strings.TrimSpace(out.String()) != "nothing to sync" {
		t.Fatalf("second sync: %q", out.String())
	}

	s.MarkAllPending(ctx)
	out.Reset()
	syncEvents(ctx, s, client, &out)
	if !strings.Contains(out.String(), "synced 503 events") {
		t.Fatalf("after marking all pending: %q", out.String())
	}
}

func TestSyncBadKey(t *testing.T) {
	ctx := context.Background()
	s, _ := store.Open(filepath.Join(t.TempDir(), "sablier.db"))
	defer s.Close()
	e, _ := event.New(event.InputTick, time.Now(), event.InputTickData{Keys: 1})
	s.Append(ctx, e)

	srv := httptest.NewServer(&fakeServer{})
	defer srv.Close()
	err := syncEvents(ctx, s, api.New(srv.URL, "sab_bad"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "login again") {
		t.Fatalf("got %v", err)
	}
	if p, _ := s.Pending(ctx, 10); len(p) != 1 {
		t.Fatal("event marked uploaded after a failed sync")
	}
}

func TestSyncNotSignedIn(t *testing.T) {
	t.Setenv("SABLIER_CONFIG", filepath.Join(t.TempDir(), "none.cfg"))
	t.Setenv("SABLIER_HOME", t.TempDir())
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"sync"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "sablier login") {
		t.Fatalf("got %v", err)
	}
}

func TestSyncNewInstallOnConflict(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "sablier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 3 {
		e, _ := event.New(event.Focus, time.Now().Add(time.Duration(i)*time.Second), event.FocusData{In: true})
		if _, err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := s.InstallID(ctx)

	var got []api.Batch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b api.Batch
		json.NewDecoder(r.Body).Decode(&b)
		got = append(got, b)
		if b.InstallID == old {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "event 1: differs from the one already received"})
			return
		}
		json.NewEncoder(w).Encode(api.Result{Accepted: len(b.Events)})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := syncEvents(ctx, s, api.New(srv.URL, "sab_good"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new install") || !strings.Contains(out.String(), "synced 3 events") {
		t.Fatalf("output: %s", out.String())
	}
	sent := got[len(got)-1]
	if sent.InstallID == old || sent.Events[0].PrevHash != "" {
		t.Fatalf("not a fresh chain on a new install: %+v", sent)
	}
	for i, e := range sent.Events {
		want, _ := event.ChainHash(e.PrevHash, e)
		if e.Hash != want || (i > 0 && e.PrevHash != sent.Events[i-1].Hash) {
			t.Fatalf("event %d doesn't chain", e.Seq)
		}
	}
}

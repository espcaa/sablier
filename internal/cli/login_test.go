package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
)

func TestDeviceLoginPollsAndSavesCredential(t *testing.T) {
	t.Setenv("SABLIER_CONFIG", filepath.Join(t.TempDir(), "config"))
	token := "sbd_" + strings.Repeat("x", 43)
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cli/token" {
			t.Error("unexpected token route")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["poll_token"] != "secret" {
			t.Error("expected the private polling token")
		}
		if polls.Add(1) == 1 {
			json.NewEncoder(w).Encode(api.DeviceCredential{Status: "pending"})
			return
		}
		json.NewEncoder(w).Encode(api.DeviceCredential{Status: "approved", Token: token, DeviceID: "fixture"})
	}))
	defer server.Close()

	cfg := config.Config{APIURL: server.URL}
	err := waitForDevice(t.Context(), NewRootCmd(), cfg, api.New(server.URL, ""), "secret", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load()
	if err != nil || cfg.APIKey != token || cfg.APIURL != server.URL || polls.Load() != 2 {
		t.Fatal("api key was not saved after approval")
	}
}

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espcaa/sablier/internal/config"
)

func runLogin(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"login"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sab_good" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"id":"ident!a1","name":"Alice B"}`))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "sablier.cfg")
	t.Setenv("SABLIER_CONFIG", cfgPath)

	if _, err := runLogin(t, "sab_bad\n", "--key", "--api-url", srv.URL); err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("config saved for a bad key")
	}

	out, err := runLogin(t, "  sab_good \n", "--key", "--api-url", srv.URL+"/")
	if err != nil || !strings.Contains(out, "signed in as Alice B") {
		t.Fatalf("got %q, %v", out, err)
	}
	c, _ := config.Load()
	if c.APIKey != "sab_good" || c.APIURL != srv.URL {
		t.Fatalf("saved %+v", c)
	}

	// the url is remembered
	if _, err := runLogin(t, "sab_good\n", "--key"); err != nil {
		t.Fatalf("second login: %v", err)
	}
}

func TestLoginEmptyKey(t *testing.T) {
	t.Setenv("SABLIER_CONFIG", filepath.Join(t.TempDir(), "sablier.cfg"))
	if _, err := runLogin(t, "\n", "--key"); err == nil || !strings.Contains(err.Error(), "no api key") {
		t.Fatalf("got %v", err)
	}
}

func TestBrowserLogin(t *testing.T) {
	var challenge string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/authorize":
			q := r.URL.Query()
			challenge = q.Get("code_challenge")
			if q.Get("code_challenge_method") != "S256" || len(challenge) != 43 {
				t.Errorf("bad authorize url %s", r.URL)
			}
			http.Redirect(w, r, "http://127.0.0.1:"+q.Get("port")+"/callback?code=the-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
		case "/cli/token":
			var in struct{ Code, Code_verifier string }
			json.NewDecoder(r.Body).Decode(&in)
			sum := sha256.Sum256([]byte(in.Code_verifier))
			if in.Code != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"code_verifier doesn't match"}`))
				return
			}
			w.Write([]byte(`{"api_key":"sab_fromweb","id":"ident!a1","name":"Alice B"}`))
		case "/cli/done":
			if r.URL.Query().Get("error") != "" {
				t.Errorf("done with error %s", r.URL)
			}
			w.Write([]byte("signed in"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("SABLIER_CONFIG", filepath.Join(t.TempDir(), "sablier.cfg"))

	opened := make(chan string, 1)
	openBrowser = func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			opened <- string(b)
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = defaultOpenBrowser })

	out, err := runLogin(t, "", "--api-url", srv.URL)
	if err != nil || !strings.Contains(out, "signed in as Alice B") {
		t.Fatalf("got %q, %v", out, err)
	}
	if page := <-opened; page != "signed in" {
		t.Fatalf("browser ended on %q", page)
	}
	c, _ := config.Load()
	if c.APIKey != "sab_fromweb" || c.APIURL != srv.URL {
		t.Fatalf("saved %+v", c)
	}
}

func TestBrowserLoginDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cli/authorize" {
			q := r.URL.Query()
			http.Redirect(w, r, "http://127.0.0.1:"+q.Get("port")+"/callback?error=access_denied&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "sablier.cfg")
	t.Setenv("SABLIER_CONFIG", cfgPath)
	openBrowser = func(u string) error {
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = defaultOpenBrowser })

	if _, err := runLogin(t, "", "--api-url", srv.URL); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("config saved after a denied login")
	}
}

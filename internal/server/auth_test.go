package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/espcaa/sablier/internal/server/db"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/hcauth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// needs SABLIER_TEST_DATABASE_URL, wipes the db
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SABLIER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SABLIER_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// a fake auth.hackclub.com whose authorize page approves right away
func fakeHackClub(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=good-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"idntk.abc"}`))
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"identity":{"id":"ident!a1","first_name":"Alice","last_name":"B","slack_id":"U123"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// where u redirects to, without following it
func redirect(t *testing.T, jar http.CookieJar, u string) string {
	t.Helper()
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header.Get("Location")
}

func TestSignInFlow(t *testing.T) {
	pool := testPool(t)
	hc := fakeHackClub(t)

	app := httptest.NewUnstartedServer(nil)
	app.Start()
	t.Cleanup(app.Close)
	auth := hcauth.New("client", "secret", app.URL+"/auth/callback")
	auth.BaseURL = hc.URL
	app.Config.Handler = New(Config{BaseURL: app.URL}, pool, sqlc.New(pool), auth)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}

	if to := redirect(t, jar, app.URL+"/app"); to != "/auth/login" {
		t.Fatalf("signed out /app redirects to %q", to)
	}

	// login -> fake hack club -> callback -> /app
	code, body := get(t, browser, app.URL+"/auth/login")
	if code != 200 || !strings.Contains(body, "cachet.dunkirk.sh/users/U123/r") {
		t.Fatalf("after sign in (%d): %s", code, body)
	}
	code, body = get(t, browser, app.URL+"/app/settings")
	if code != 200 || !strings.Contains(body, "No connected devices") {
		t.Fatalf("settings (%d): %s", code, body)
	}

	resp, err := browser.PostForm(app.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if to := redirect(t, jar, app.URL+"/app"); to != "/auth/login" {
		t.Fatalf("after sign out /app redirects to %q", to)
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	s := New(Config{}, nil, nil, hcauth.New("client", "secret", "http://x/auth/callback"))

	req := httptest.NewRequest("GET", "/auth/callback?code=c&state=forged", nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: "real"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/callback?code=c&state=x", nil)) // no cookie
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no state cookie: got %d", rec.Code)
	}
}

func TestSignInOff(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Config{}, nil, nil, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestLoginSetsState(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Config{}, nil, nil, hcauth.New("client", "secret", "http://x/auth/callback")).
		ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))

	loc, _ := url.Parse(rec.Header().Get("Location"))
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusFound || len(cookies) != 1 || cookies[0].Name != stateCookie ||
		loc.Query().Get("state") != cookies[0].Value || !cookies[0].HttpOnly {
		t.Fatalf("got %d, %s, %+v", rec.Code, loc, cookies)
	}
}

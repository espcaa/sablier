package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/hcauth"
	"github.com/jackc/pgx/v5/pgtype"
)

func cliQuery(port, state, verifier string) url.Values {
	sum := sha256.Sum256([]byte(verifier))
	return url.Values{
		"port":                  {port},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
}

func postToken(t *testing.T, base, code, verifier string) (int, map[string]string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	resp, err := http.Post(base+"/cli/token", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestCLILoginFlow(t *testing.T) {
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
	const verifier = "a-long-random-verifier-from-the-cli-0123456789"
	q := cliQuery("45678", "state-from-the-cli-123", verifier)

	code, body := get(t, browser, app.URL+"/cli/authorize?"+q.Encode())
	if code != 200 || !strings.Contains(body, "Link the CLI to your account?") || !strings.Contains(body, "Alice") {
		t.Fatalf("consent page (%d): %s", code, body)
	}

	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{"decision": {"allow"}}
	for k, v := range q {
		form[k] = v
	}
	resp, err := noFollow.PostForm(app.URL+"/cli/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "127.0.0.1:45678" || loc.Path != "/callback" || loc.Query().Get("state") != "state-from-the-cli-123" {
		t.Fatalf("allow redirects to %s", loc)
	}
	cliCode := loc.Query().Get("code")

	if st, _ := postToken(t, app.URL, cliCode, "not-the-verifier"); st != 400 {
		t.Fatalf("wrong verifier: %d", st)
	}
	if st, _ := postToken(t, app.URL, cliCode, verifier); st != 400 {
		t.Fatalf("burnt code still works: %d", st)
	}

	resp, _ = noFollow.PostForm(app.URL+"/cli/authorize", form)
	resp.Body.Close()
	loc, _ = url.Parse(resp.Header.Get("Location"))
	st, out := postToken(t, app.URL, loc.Query().Get("code"), verifier)
	if st != 200 || !strings.HasPrefix(out["api_key"], "sab_") || out["name"] != "Alice B" {
		t.Fatalf("token (%d): %v", st, out)
	}
	u, _ := sqlc.New(pool).UserByID(context.Background(), "ident!a1")
	if out["api_key"] != u.ApiKey {
		t.Fatal("got a different key than the dashboard's")
	}

	code, body = get(t, browser, app.URL+"/cli/done")
	if code != 200 || !strings.Contains(body, "You're signed in!") {
		t.Fatalf("done page (%d): %s", code, body)
	}
}

func TestCLIDeny(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: "ident!a1", Name: "Alice B", ApiKey: "sab_good"})
	h := New(Config{}, pool, q, nil)

	session := randomToken()
	q.CreateSession(ctx, sqlc.CreateSessionParams{TokenHash: hash(session), UserID: "ident!a1", ExpiresAt: pgFuture()})

	form := cliQuery("45678", "state-from-the-cli-123", "v")
	form.Set("decision", "deny")
	req := httptest.NewRequest("POST", "/cli/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny: %d %s", rec.Code, loc)
	}
}

func TestCLIAuthorizeRejectsBadRequests(t *testing.T) {
	h := New(Config{}, nil, nil, nil)
	good := cliQuery("45678", "state-from-the-cli-123", "v")
	for name, mod := range map[string]func(url.Values){
		"low port":     func(v url.Values) { v.Set("port", "80") },
		"not a port":   func(v url.Values) { v.Set("port", "evil.example") },
		"short state":  func(v url.Values) { v.Set("state", "x") },
		"plain pkce":   func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"no challenge": func(v url.Values) { v.Del("code_challenge") },
	} {
		v := url.Values{}
		for k, x := range good {
			v[k] = append([]string(nil), x...)
		}
		mod(v)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/cli/authorize?"+v.Encode(), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
}

func TestCLIDoneOnlyKnownErrors(t *testing.T) {
	h := New(Config{}, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/cli/done?error=%3Cscript%3Eyour+account+is+hacked", nil))
	if strings.Contains(rec.Body.String(), "hacked") || !strings.Contains(rec.Body.String(), "Something went wrong") {
		t.Fatalf("got %s", rec.Body.String())
	}
}

func pgFuture() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
}

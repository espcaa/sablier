package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/hcauth"
)

func TestDeviceLoginAndRevocation(t *testing.T) {
	pool := testPool(t)
	hc := fakeHackClub(t)
	app := httptest.NewUnstartedServer(nil)
	app.Start()
	defer app.Close()
	auth := hcauth.New("client", "secret", app.URL+"/auth/callback")
	auth.BaseURL = hc.URL
	app.Config.Handler = New(Config{BaseURL: app.URL}, pool, sqlc.New(pool), auth)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	client := api.New(app.URL, "")
	ctx := context.Background()
	login, err := client.StartDeviceLogin(ctx, "work-laptop")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request": {login.Approval}}
	status, body := get(t, browser, app.URL+"/cli/authorize?"+form.Encode())
	if status != http.StatusOK || !strings.Contains(body, login.Approval) || strings.Contains(body, login.PollToken) {
		t.Fatalf("unexpected consent page, status %d", status)
	}
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(path string, values url.Values, origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, app.URL+path, strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	poll := func(token, want string) api.DeviceCredential {
		t.Helper()
		result, err := client.PollDeviceLogin(ctx, token)
		if err != nil || result.Status != want {
			t.Fatalf("poll status %q, want %q, error %v", result.Status, want, err)
		}
		return result
	}
	decide := func(request api.DeviceLogin, decision, location string) {
		t.Helper()
		form := url.Values{"request": {request.Approval}, "decision": {decision}}
		res := post("/cli/authorize", form, app.URL)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != location {
			t.Fatalf("unexpected decision response: %d", res.StatusCode)
		}
	}
	poll(login.PollToken, "pending")
	poll(login.Approval, "expired")
	decide(login, "allow", "/cli/done")
	first := poll(login.PollToken, "approved")
	poll(login.PollToken, "expired")

	next, err := client.StartDeviceLogin(ctx, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	decide(next, "allow", "/cli/done")
	second := poll(next.PollToken, "approved")
	if first.Token == second.Token {
		t.Fatal("logins share a credential")
	}

	denied, err := client.StartDeviceLogin(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	decide(denied, "deny", "/")
	poll(denied.PollToken, "denied")

	status, body = get(t, browser, app.URL+"/app/settings")
	if status != http.StatusOK || !strings.Contains(body, "work-laptop") || !strings.Contains(body, "desktop") {
		t.Fatal("device hostnames missing from settings")
	}
	revoke := url.Values{"id": {first.DeviceID}}
	if res := post("/app/settings/revoke", revoke, "https://example.com"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin revocation returned %d", res.StatusCode)
	}
	if _, err := api.New(app.URL, first.Token).Me(ctx); err != nil {
		t.Fatal(err)
	}
	if res := post("/app/settings/revoke", revoke, app.URL); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke returned %d", res.StatusCode)
	}
	if _, err := api.New(app.URL, first.Token).Me(ctx); err != api.ErrUnauthorized {
		t.Fatal("revoked credential accepted")
	}
	if _, err := api.New(app.URL, second.Token).Me(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := api.New(app.URL, "sab_legacy").Me(ctx); err != api.ErrUnauthorized {
		t.Fatal("legacy key accepted")
	}
}

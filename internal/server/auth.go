package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	sessionCookie = "sablier_session"
	stateCookie   = "sablier_oauth_state"
	nextCookie    = "sablier_next"
	sessionTTL    = 30 * 24 * time.Hour
)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if s.currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	if s.auth == nil {
		http.Error(w, "sign in is not configured", http.StatusServiceUnavailable)
		return
	}
	if next != "/app" {
		http.SetCookie(w, s.cookie(nextCookie, url.QueryEscape(next), "/auth", 10*time.Minute))
	}
	// checked on callback, so a login can't be started by another site
	state := randomToken()
	http.SetCookie(w, s.cookie(stateCookie, state, "/auth", 10*time.Minute))
	http.Redirect(w, r, s.auth.AuthorizeURL(state), http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Error(w, "sign in is not configured", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "sign in expired, try again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, s.cookie(stateCookie, "", "/auth", -1))

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "sign in was cancelled", http.StatusBadRequest)
		return
	}

	// the hack club token is only used here, never stored
	token, err := s.auth.Exchange(r.Context(), code)
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't reach hack club auth", http.StatusBadGateway)
		return
	}
	id, err := s.auth.Me(r.Context(), token)
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't reach hack club auth", http.StatusBadGateway)
		return
	}

	u, err := s.q.UpsertUser(r.Context(), sqlc.UpsertUserParams{
		ID:      id.ID,
		SlackID: pgtype.Text{String: id.SlackID, Valid: id.SlackID != ""},
		Name:    id.Name(),
		ApiKey:  newAPIKey(), // only used if the user is new
	})
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't save user", http.StatusInternalServerError)
		return
	}

	session := randomToken()
	err = s.q.CreateSession(r.Context(), sqlc.CreateSessionParams{
		TokenHash: hash(session),
		UserID:    u.ID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(sessionTTL), Valid: true},
	})
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't start session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, session, "/", sessionTTL))
	next := "/app"
	if c, err := r.Cookie(nextCookie); err == nil {
		if v, err := url.QueryUnescape(c.Value); err == nil {
			next = localPath(v)
		}
		http.SetCookie(w, s.cookie(nextCookie, "", "/auth", -1))
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/app"
	}
	return p
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.q.DeleteSession(r.Context(), hash(c.Value))
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", "/", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// nil when signed out
func (s *Server) currentUser(r *http.Request) *sqlc.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || s.q == nil {
		return nil
	}
	u, err := s.q.UserBySession(r.Context(), hash(c.Value))
	if err != nil {
		return nil
	}
	return &u
}

// maxAge < 0 deletes the cookie
func (s *Server) cookie(name, value, path string, maxAge time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode, // also blocks cross-site posts like logout
	}
	if maxAge < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge.Seconds())
	}
	return c
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func newAPIKey() string {
	b := make([]byte, 20)
	rand.Read(b)
	return "sab_" + hex.EncodeToString(b)
}

func hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/views"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// `sablier login` in the browser, like oauth for native apps (rfc 8252):
//  1. the cli listens on 127.0.0.1:port and opens /cli/authorize?port=&state=&code_challenge=
//  2. the user signs in and allows it, the browser goes to 127.0.0.1:port/callback?code=&state=
//  3. the cli posts the code and its pkce verifier to /cli/token and gets the api key
//  4. the cli sends the browser to /cli/done
const cliCodeTTL = 2 * time.Minute

type cliRequest struct {
	port      int
	state     string
	challenge string
}

func parseCLIRequest(v url.Values) (cliRequest, bool) {
	port, err := strconv.Atoi(v.Get("port"))
	req := cliRequest{port: port, state: v.Get("state"), challenge: v.Get("code_challenge")}
	ok := err == nil && port >= 1024 && port <= 65535 &&
		len(req.state) >= 16 && len(req.state) <= 128 &&
		len(req.challenge) == 43 && // base64url sha256, no padding
		v.Get("code_challenge_method") == "S256"
	return req, ok
}

func (c cliRequest) callback(q url.Values) string {
	q.Set("state", c.state)
	return "http://127.0.0.1:" + strconv.Itoa(c.port) + "/callback?" + q.Encode()
}

func (s *Server) cliAuthorize(w http.ResponseWriter, r *http.Request) {
	req, ok := parseCLIRequest(r.URL.Query())
	if !ok {
		http.Error(w, "this sign in link is broken, run sablier login again", http.StatusBadRequest)
		return
	}
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	w.Header().Set("X-Frame-Options", "DENY") // no clickjacking the allow button
	w.Header().Set("Cache-Control", "no-store")
	views.CLIConsent(views.Consent{
		Name:      u.Name,
		Port:      strconv.Itoa(req.port),
		State:     req.state,
		Challenge: req.challenge,
	}).Render(r.Context(), w)
}

// the form posts here. SameSite=Lax on the session cookie blocks cross-site posts
func (s *Server) cliDecide(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req, ok := parseCLIRequest(r.PostForm)
	if !ok {
		http.Error(w, "this sign in link is broken, run sablier login again", http.StatusBadRequest)
		return
	}
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, "signed out, run sablier login again", http.StatusUnauthorized)
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		http.Redirect(w, r, req.callback(url.Values{"error": {"access_denied"}}), http.StatusSeeOther)
		return
	}

	code := randomToken()
	err := s.q.CreateCLICode(r.Context(), sqlc.CreateCLICodeParams{
		CodeHash:  hash(code),
		UserID:    u.ID,
		Challenge: req.challenge,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(cliCodeTTL), Valid: true},
	})
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't save the sign in, try again", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, req.callback(url.Values{"code": {code}}), http.StatusSeeOther)
}

// the cli trades the code for the api key
func (s *Server) cliToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Code == "" || in.Verifier == "" {
		writeError(w, http.StatusBadRequest, "code and code_verifier are required")
		return
	}
	row, err := s.q.ConsumeCLICode(r.Context(), hash(in.Code))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "invalid or used code")
		return
	}
	if err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "couldn't check the code")
		return
	}
	if time.Now().After(row.ExpiresAt.Time) {
		writeError(w, http.StatusBadRequest, "code expired")
		return
	}
	if subtle.ConstantTimeCompare([]byte(pkceChallenge(in.Verifier)), []byte(row.Challenge)) != 1 {
		writeError(w, http.StatusBadRequest, "code_verifier doesn't match")
		return
	}
	u, err := s.q.UserByID(r.Context(), row.UserID)
	if err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "couldn't load the user")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"api_key": u.ApiKey, "id": u.ID, "name": u.Name})
}

func (s *Server) cliDone(w http.ResponseWriter, r *http.Request) {
	// only known messages, the query string is easy to fake
	var d views.Done
	switch r.URL.Query().Get("error") {
	case "":
	case "access_denied":
		d.Error = "You cancelled it"
	default:
		d.Error = "Something went wrong"
	}
	views.CLIDone(d).Render(r.Context(), w)
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

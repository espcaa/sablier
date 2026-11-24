package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/views"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const cliLoginCookie = "sablier_cli_login"

var cliSecret = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	protection := http.NewCrossOriginProtection()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "" || protection.Check(r) != nil {
			http.Error(w, "invalid request origin", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) startDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Hostname string }
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sign-in request")
		return
	}
	ctx := r.Context()
	approval, poll := randomToken(), randomToken()
	if err := s.q.DeleteExpiredDeviceLogins(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	err := s.q.CreateDeviceLogin(ctx, sqlc.CreateDeviceLoginParams{
		ApprovalHash: hash(approval), PollHash: hash(poll), Hostname: deviceHostname(in.Hostname),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceLogin{Approval: approval, PollToken: poll})
}

func (s *Server) authorizeCLI(w http.ResponseWriter, r *http.Request) {
	approval := r.URL.Query().Get("request")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if !cliSecret.MatchString(approval) {
		http.Error(w, "invalid sign-in request", http.StatusBadRequest)
		return
	}
	hostname, err := s.q.PendingDeviceLogin(r.Context(), hash(approval))
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "sign-in request expired or already handled", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "could not load sign-in request", http.StatusInternalServerError)
		return
	}
	if s.currentUser(r) == nil {
		http.SetCookie(w, s.cookie(cliLoginCookie, approval, "/auth", 5*time.Minute))
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	views.DeviceConsent(approval, hostname).Render(r.Context(), w)
}

func (s *Server) approveCLI(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil || !cliSecret.MatchString(r.PostForm.Get("request")) {
		http.Error(w, "invalid sign-in request", http.StatusBadRequest)
		return
	}
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, "sign in again", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	allow := r.PostForm.Get("decision") == "allow"
	n, err := s.q.DecideDeviceLogin(r.Context(), sqlc.DecideDeviceLoginParams{
		ApprovalHash: hash(r.PostForm.Get("request")), UserID: pgtype.Text{String: u.ID, Valid: allow}, Denied: !allow,
	})
	if err != nil {
		http.Error(w, "could not save decision", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "sign-in request expired or already handled", http.StatusGone)
		return
	}
	if allow {
		http.Redirect(w, r, "/cli/done", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) redeemCLI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PollToken string `json:"poll_token"`
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || !cliSecret.MatchString(in.PollToken) {
		writeError(w, http.StatusBadRequest, "invalid sign-in request")
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	pollHash := hash(in.PollToken)
	row, err := q.PollDeviceLogin(ctx, pollHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(w, http.StatusOK, api.DeviceCredential{Status: "expired"})
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	case row.Denied:
		writeJSON(w, http.StatusOK, api.DeviceCredential{Status: "denied"})
		return
	case !row.UserID.Valid:
		writeJSON(w, http.StatusOK, api.DeviceCredential{Status: "pending"})
		return
	}
	id := uuid.New()
	token := "sbd_" + randomToken()
	err = q.CreateDeviceCredential(ctx, sqlc.CreateDeviceCredentialParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, UserID: row.UserID.String, TokenHash: hash(token), Hostname: row.Hostname,
	})
	if err != nil || q.ConsumeDeviceLogin(ctx, pollHash) != nil || tx.Commit(ctx) != nil {
		writeError(w, http.StatusInternalServerError, "could not save device")
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceCredential{Status: "approved", Token: token, DeviceID: id.String()})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, "sign in again", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PostFormValue("id"))
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	if err := s.q.RevokeDeviceCredential(r.Context(), sqlc.RevokeDeviceCredentialParams{ID: pgtype.UUID{Bytes: id, Valid: true}, UserID: u.ID}); err != nil {
		http.Error(w, "could not revoke device", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/settings", http.StatusSeeOther)
}

func (s *Server) deviceDone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	views.DeviceDone().Render(r.Context(), w)
}

func deviceHostname(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 255 || strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return ""
	}
	return name
}

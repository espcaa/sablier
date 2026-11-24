package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/espcaa/sablier/internal/server/db/sqlc"
)

type userKey struct{}

// api requests carry the user's key: Authorization: Bearer sab_...
func (s *Server) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeError(w, http.StatusUnauthorized, "missing api key")
			return
		}
		u, err := s.q.UserByDeviceToken(r.Context(), hash(key))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, &u)))
	})
}

func apiUser(r *http.Request) *sqlc.User { return r.Context().Value(userKey{}).(*sqlc.User) }

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := apiUser(r)
	writeJSON(w, http.StatusOK, map[string]string{"id": u.ID, "name": u.Name})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

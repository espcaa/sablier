package server

import (
	"log"
	"net/http"
	"slices"

	"github.com/espcaa/sablier/internal/server/views"
)

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if len(s.cfg.AdminUserIDs) == 0 {
		http.NotFound(w, r)
		return
	}
	user := s.currentUser(r)
	if user == nil {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	if !slices.Contains(s.cfg.AdminUserIDs, user.ID) {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	stats, err := s.q.AdminStats(r.Context())
	if err != nil {
		log.Printf("load admin stats: %v", err)
		http.Error(w, "could not load admin stats", http.StatusInternalServerError)
		return
	}
	views.Admin(views.Me{Name: user.Name, SlackID: user.SlackID.String}, stats).Render(r.Context(), w)
}

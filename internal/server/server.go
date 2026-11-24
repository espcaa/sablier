package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/espcaa/sablier/internal/server/db"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/espcaa/sablier/internal/server/hcauth"
	"github.com/espcaa/sablier/internal/server/views"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type database interface {
	Ping(ctx context.Context) error
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Server struct {
	cfg  Config
	db   database
	q    *sqlc.Queries
	auth *hcauth.Client
}

func New(cfg Config, db database, q *sqlc.Queries, auth *hcauth.Client) http.Handler {
	s := &Server{cfg: cfg, db: db, q: q, auth: auth}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	r.Post("/cli/start", s.startDeviceLogin)
	r.Get("/cli/authorize", s.authorizeCLI)
	r.Post("/cli/authorize", sameOrigin(s.approveCLI))
	r.Post("/cli/token", s.redeemCLI)
	r.Get("/cli/done", s.deviceDone)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		r.Get("/health", s.health)
		r.Get("/app", s.app)
		r.Get("/app/settings", s.settings)
		r.Post("/app/settings/revoke", sameOrigin(s.revokeDevice))
		r.Get("/auth/login", s.login)
		r.Get("/auth/callback", s.callback)
		r.Post("/auth/logout", s.logout)

		r.Route("/v1", func(r chi.Router) {
			r.Use(s.requireKey)
			r.Get("/me", s.me)
			r.Post("/events", s.ingest)
			r.Get("/verify", s.verificationCheckpoint)
			r.Post("/verify", s.receivePrompts)
		})
	})
	r.NotFound(siteHandler(cfg.SiteDir, cfg.SiteProxy).ServeHTTP)
	return r
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *Server) app(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	now := time.Now().In(viewerLocation(r))
	today, err := s.summary(r.Context(), u.ID, midnight(now), now)
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't load today", http.StatusInternalServerError)
		return
	}
	all, err := s.summary(r.Context(), u.ID, time.Time{}, now)
	if err != nil {
		log.Print(err)
		http.Error(w, "couldn't load your projects", http.StatusInternalServerError)
		return
	}
	views.App(views.Me{Name: u.Name, SlackID: u.SlackID.String, Today: today, AllTime: all}).Render(r.Context(), w)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	rows, err := s.q.DeviceCredentials(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "could not load devices", http.StatusInternalServerError)
		return
	}
	devices := make([]views.Device, 0, len(rows))
	for _, row := range rows {
		devices = append(devices, views.Device{ID: row.ID.String(), Hostname: row.Hostname, Created: row.CreatedAt.Time.UTC().Format("2 Jan 2006, 15:04 UTC")})
	}
	views.Settings(views.Me{Name: u.Name, SlackID: u.SlackID.String}, devices).Render(r.Context(), w)
}

// serves until ctx is done, then shuts down gracefully
func Run(ctx context.Context, c Config) error {
	pool, err := pgxpool.New(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	var auth *hcauth.Client
	if c.HackClubClientID != "" {
		auth = hcauth.New(c.HackClubClientID, c.HackClubClientSecret, c.BaseURL+"/auth/callback")
	} else {
		log.Print("HACKCLUB_CLIENT_ID not set, sign in is off")
	}

	srv := &http.Server{Addr: c.Addr, Handler: New(c, pool, sqlc.New(pool), auth), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("sablier server on %s", c.Addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

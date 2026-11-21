package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/project"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	var b api.Batch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	var install pgtype.UUID
	if err := install.Scan(b.InstallID); err != nil {
		writeError(w, http.StatusBadRequest, "bad install_id")
		return
	}
	if len(b.Events) > api.MaxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d events per batch", api.MaxBatch))
		return
	}
	now := time.Now()
	for _, e := range b.Events {
		if err := validate(e, now); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("event %d: %v", e.Seq, err))
			return
		}
	}
	for _, p := range b.Projects {
		if !validProjectID.MatchString(p.ID) || !project.ValidName(p.Name) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("project %q: name must be a folder name, not a path", p.ID))
			return
		}
	}

	u := apiUser(r)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback(r.Context())
	q := s.q.WithTx(tx)

	_, err = q.ClaimInstall(r.Context(), sqlc.ClaimInstallParams{ID: install, UserID: u.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "this install belongs to another account")
		return
	}
	if err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	head, err := q.InstallHead(r.Context(), install)
	if err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	var res api.Result
	moved := false
	for _, e := range b.Events {
		code, msg := s.store(r, q, install, &head, e, &res)
		if code == http.StatusInternalServerError {
			log.Print(msg)
			writeError(w, code, "db error")
			return
		}
		if code != 0 {
			log.Printf("install %s: %s", b.InstallID, msg)
			writeError(w, code, msg)
			return
		}
		moved = moved || e.Hash != ""
	}
	if moved {
		err := q.SetInstallHead(r.Context(), sqlc.SetInstallHeadParams{ID: install, HeadSeq: head.HeadSeq, HeadHash: head.HeadHash})
		if err != nil {
			log.Print(err)
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
	}
	for _, p := range b.Projects {
		err := q.UpsertProject(r.Context(), sqlc.UpsertProjectParams{InstallID: install, ProjectID: p.ID, Name: p.Name})
		if err != nil {
			log.Print(err)
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Print(err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// how far ahead of the server's clock an event can be
const maxClockSkew = 5 * time.Minute

func validate(e event.Event, now time.Time) error {
	switch {
	case e.Seq <= 0:
		return errors.New("seq must be positive")
	case e.TS.IsZero():
		return errors.New("missing ts")
	case e.TS.After(now.Add(maxClockSkew)):
		return errors.New("ts is in the future, check this machine's clock")
	case !e.Kind.Valid():
		return fmt.Errorf("unknown kind %q", e.Kind)
	case len(e.Data) > 0 && !json.Valid(e.Data):
		return errors.New("data is not json")
	}
	return nil
}

// stores one event and moves head along the chain. a non zero code rejects the whole batch.
// events without a hash come from clients before the chain and are kept unchained
func (s *Server) store(r *http.Request, q *sqlc.Queries, install pgtype.UUID, head *sqlc.InstallHeadRow, e event.Event, res *api.Result) (int, string) {
	ctx := r.Context()
	chained := e.Hash != ""
	if !chained && head.HeadSeq.Valid && e.Seq > head.HeadSeq.Int64 {
		return http.StatusConflict, fmt.Sprintf("event %d: no hash, but this install's chain already started", e.Seq)
	}
	if chained {
		if want, err := event.ChainHash(e.PrevHash, e); err != nil || want != e.Hash {
			return http.StatusBadRequest, fmt.Sprintf("event %d: hash doesn't match its content", e.Seq)
		}
		if head.HeadSeq.Valid && e.Seq <= head.HeadSeq.Int64 {
			// already received, fine if it's the exact same event
			stored, err := q.StoredEventHash(ctx, sqlc.StoredEventHashParams{InstallID: install, Seq: e.Seq})
			if err == nil && stored.String == e.Hash {
				res.Duplicates++
				return 0, ""
			}
			return http.StatusConflict, fmt.Sprintf("event %d: differs from the one already received", e.Seq)
		}
		if head.HeadSeq.Valid && (e.Seq != head.HeadSeq.Int64+1 || e.PrevHash != head.HeadHash.String) {
			return http.StatusConflict, fmt.Sprintf("event %d: doesn't continue from event %d", e.Seq, head.HeadSeq.Int64)
		}
		if !head.HeadSeq.Valid && e.PrevHash != "" {
			return http.StatusConflict, fmt.Sprintf("event %d: links to an event the server never got", e.Seq)
		}
	}

	data := []byte(e.Data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	n, err := q.InsertEvent(ctx, sqlc.InsertEventParams{
		InstallID: install,
		Seq:       e.Seq,
		Ts:        pgtype.Timestamptz{Time: e.TS, Valid: true},
		Kind:      string(e.Kind),
		SessionID: e.SessionID,
		ProjectID: e.ProjectID,
		Harness:   e.Harness,
		Source:    string(e.Source),
		Data:      data,
		PrevHash:  pgtype.Text{String: e.PrevHash, Valid: chained},
		Hash:      pgtype.Text{String: e.Hash, Valid: chained},
	})
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	if n == 0 {
		if chained {
			// an unchained event already has this seq
			return http.StatusConflict, fmt.Sprintf("event %d: differs from the one already received", e.Seq)
		}
		res.Duplicates++
		return 0, ""
	}
	res.Accepted++
	if chained {
		head.HeadSeq = pgtype.Int8{Int64: e.Seq, Valid: true}
		head.HeadHash = pgtype.Text{String: e.Hash, Valid: true}
	}
	return 0, ""
}

var validProjectID = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,300}$`)

package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func verificationScope(project, install string) (pgtype.UUID, bool) {
	id, err := uuid.Parse(install)
	return pgtype.UUID{Bytes: id, Valid: err == nil}, err == nil && validProjectID.MatchString(project)
}

func (s *Server) verificationCheckpoint(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	id, ok := verificationScope(project, r.URL.Query().Get("install"))
	if !ok {
		writeError(w, 400, "invalid project or install")
		return
	}
	row, err := s.q.VerificationCheckpoint(r.Context(), sqlc.VerificationCheckpointParams{UserID: apiUser(r).ID, InstallID: id, ProjectID: project})
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 200, api.Checkpoint{})
		return
	}
	if err != nil {
		writeError(w, 500, "cannot read checkpoint")
		return
	}
	writeJSON(w, 200, api.Checkpoint{LastSeq: row.LastSeq, ReceivedAt: &row.ReceivedAt.Time})
}

func (s *Server) receivePrompts(w http.ResponseWriter, r *http.Request) {
	var b api.PromptBatch
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, api.VerifyBodyLimit))
	if err := d.Decode(&b); err != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		writeError(w, 400, "invalid prompt batch")
		return
	}
	id, ok := verificationScope(b.Project, b.Install)
	if !ok || len(b.Prompts) == 0 || len(b.Prompts) > api.VerifyBatchSize {
		writeError(w, 400, "invalid prompt batch")
		return
	}
	var last int64
	for _, p := range b.Prompts {
		if p.Seq <= last || p.Text == "" {
			writeError(w, 400, "expected ordered, nonempty prompts")
			return
		}
		last = p.Seq
	}
	b.Prompts = nil
	row, err := s.q.AdvanceVerification(r.Context(), sqlc.AdvanceVerificationParams{UserID: apiUser(r).ID, InstallID: id, ProjectID: b.Project, LastSeq: last})
	if err != nil {
		writeError(w, 500, "cannot save checkpoint")
		return
	}
	writeJSON(w, 200, api.Checkpoint{LastSeq: row.LastSeq, ReceivedAt: &row.ReceivedAt.Time})
}

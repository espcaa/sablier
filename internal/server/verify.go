package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/espcaa/sablier/internal/api"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func verificationScope(project, install string) (pgtype.UUID, bool) {
	id, err := uuid.Parse(install)
	return pgtype.UUID{Bytes: id, Valid: err == nil}, err == nil && validProjectID.MatchString(project)
}

func (s *Server) receivePrompts(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DecisionAPIKey == "" || s.cfg.DecisionAPIURL == "" {
		writeError(w, http.StatusServiceUnavailable, "verification model is not configured")
		return
	}
	var b api.PromptBatch
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, api.VerifyBodyLimit))
	if err := d.Decode(&b); err != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid prompt batch")
		return
	}
	id, ok := verificationScope(b.Project, b.Install)
	if !ok || b.ThroughSeq <= 0 || len(b.Prompts) > api.MaxReviewPrompts {
		writeError(w, http.StatusBadRequest, "expected a server-selected review sample, update sablier if needed")
		return
	}
	var last int64
	for _, p := range b.Prompts {
		if p.Seq <= last || p.Seq > b.ThroughSeq {
			writeError(w, http.StatusBadRequest, "expected ordered prompts within the selected sample")
			return
		}
		last = p.Seq
	}
	result, err := s.analyzePrompts(r.Context(), apiUser(r).ID, id, b)
	switch {
	case errors.Is(err, errReviewBudget):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errReviewProvider):
		writeError(w, http.StatusBadGateway, "verification analysis unavailable, rerun verify to retry")
	case errors.Is(err, errReviewPlan):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not save analysis")
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

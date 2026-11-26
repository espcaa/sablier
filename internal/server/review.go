package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// lifetime cap on sampled prompts per user, project and active utc hour, so one busy hour can't fill the sample
const reviewPerHour = 10

const labelUnavailable = "unavailable"

var errReviewPlan = errors.New("review sample changed, rerun verify to resume")

func reviewHour(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC().Truncate(time.Hour), Valid: true}
}

func verificationScope(project, install string) (pgtype.UUID, bool) {
	id, err := uuid.Parse(install)
	return pgtype.UUID{Bytes: id, Valid: err == nil}, err == nil && validProjectID.MatchString(project)
}

func (s *Server) reviewPlan(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	install, ok := verificationScope(project, r.URL.Query().Get("install"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid project or install")
		return
	}
	plan, err := s.prepareReview(r.Context(), sqlc.CurrentReviewPlanParams{UserID: apiUser(r).ID, InstallID: install, ProjectID: project})
	if err != nil {
		log.Printf("prepare review: %v", err)
		writeError(w, http.StatusInternalServerError, "could not prepare review sample")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) receivePrompts(w http.ResponseWriter, r *http.Request) {
	var b api.PromptBatch
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, api.VerifyBodyLimit))
	if err := d.Decode(&b); err != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid prompt batch")
		return
	}
	install, ok := verificationScope(b.Project, b.Install)
	if !ok || b.ThroughSeq <= 0 || len(b.Prompts) > api.MaxReviewPrompts {
		writeError(w, http.StatusBadRequest, "expected a server-selected review sample, update sablier if needed")
		return
	}
	scope := sqlc.CurrentReviewPlanParams{UserID: apiUser(r).ID, InstallID: install, ProjectID: b.Project}
	result, err := s.analyzePrompts(r.Context(), scope, b)
	switch {
	case errors.Is(err, errReviewPlan):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		log.Printf("analyze prompts: %v", err)
		writeError(w, http.StatusBadGateway, "verification failed, rerun verify to retry")
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

func (s *Server) reviewInstalls(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if !validProjectID.MatchString(project) {
		writeError(w, http.StatusBadRequest, "invalid project")
		return
	}
	rows, err := s.q.ReviewInstalls(r.Context(), sqlc.ReviewInstallsParams{UserID: apiUser(r).ID, ProjectID: project})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load review installs")
		return
	}
	result := make([]api.ReviewInstall, len(rows))
	for i, row := range rows {
		result[i] = api.ReviewInstall{Install: row.InstallID.String(), Seq: row.Seq, Hash: row.Hash.String}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// returns the open plan, or samples up to 50 prompts from the next 500 unreviewed ones (10%)
func (s *Server) prepareReview(ctx context.Context, scope sqlc.CurrentReviewPlanParams) (api.ReviewPlan, error) {
	current, err := s.q.CurrentReviewPlan(ctx, scope)
	if err == nil {
		return api.ReviewPlan{ThroughSeq: current.ThroughSeq, Sequences: current.Sequences}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return api.ReviewPlan{}, err
	}
	after, err := s.checkpoint(ctx, scope)
	if err != nil {
		return api.ReviewPlan{}, err
	}
	rows, err := s.q.ReviewCandidates(ctx, sqlc.ReviewCandidatesParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, AfterSeq: after})
	if err != nil || len(rows) == 0 {
		return api.ReviewPlan{}, err
	}
	hours := make([]pgtype.Timestamptz, len(rows))
	for i, row := range rows {
		hours[i] = reviewHour(row.Ts.Time)
	}
	counts, err := s.q.ReviewHourCounts(ctx, sqlc.ReviewHourCountsParams{UserID: scope.UserID, ProjectID: scope.ProjectID, Hours: hours})
	if err != nil {
		return api.ReviewPlan{}, err
	}
	used := map[time.Time]int64{}
	for _, c := range counts {
		used[c.EventHour.Time.UTC()] = c.Used
	}
	sequences := []int64{}
	for _, i := range rand.Perm(len(rows)) {
		hour := hours[i].Time
		if used[hour] >= reviewPerHour {
			continue
		}
		used[hour]++
		if sequences = append(sequences, rows[i].Seq); len(sequences) == api.MaxReviewPrompts {
			break
		}
	}
	slices.Sort(sequences)
	err = s.q.CreateReviewPlan(ctx, sqlc.CreateReviewPlanParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, ThroughSeq: rows[len(rows)-1].Seq, Sequences: sequences})
	if err != nil {
		return api.ReviewPlan{}, err
	}
	// a concurrent call may have created the plan first
	return s.prepareReview(ctx, scope)
}

func (s *Server) checkpoint(ctx context.Context, scope sqlc.CurrentReviewPlanParams) (int64, error) {
	seq, err := s.q.AnalysisCheckpoint(ctx, sqlc.AnalysisCheckpointParams(scope))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

func (s *Server) analyzePrompts(ctx context.Context, scope sqlc.CurrentReviewPlanParams, b api.PromptBatch) (api.ReviewResult, error) {
	after, err := s.checkpoint(ctx, scope)
	if err != nil || b.ThroughSeq <= after {
		return api.ReviewResult{AnalyzedSeq: after}, err
	}
	plan, err := s.q.CurrentReviewPlan(ctx, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ReviewResult{}, errReviewPlan
	}
	if err != nil {
		return api.ReviewResult{}, err
	}
	sent := make([]int64, len(b.Prompts))
	for i, p := range b.Prompts {
		sent[i] = p.Seq
	}
	if plan.ThroughSeq != b.ThroughSeq || !slices.Equal(plan.Sequences, sent) {
		return api.ReviewResult{}, errReviewPlan
	}
	signals, err := s.classify(ctx, scope, b)
	if err != nil {
		return api.ReviewResult{}, err
	}

	// the model call happens outside the transaction, claiming the plan makes the save happen once
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return api.ReviewResult{}, err
	}
	defer tx.Rollback(context.Background())
	q := s.q.WithTx(tx)
	claimed, err := q.ClaimReviewPlan(ctx, sqlc.ClaimReviewPlanParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, ThroughSeq: plan.ThroughSeq})
	if err != nil {
		return api.ReviewResult{}, err
	}
	if claimed == 0 {
		after, err := s.checkpoint(ctx, scope)
		return api.ReviewResult{AnalyzedSeq: after}, err
	}
	if err := q.AdvanceAnalysis(ctx, sqlc.AdvanceAnalysisParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, LastSeq: plan.ThroughSeq}); err != nil {
		return api.ReviewResult{}, err
	}
	result := api.ReviewResult{AnalyzedSeq: plan.ThroughSeq}
	for _, row := range signals {
		if err := q.SaveReviewSignal(ctx, row); err != nil {
			return api.ReviewResult{}, err
		}
		if row.Label == labelUnavailable {
			result.Unavailable++
		} else {
			result.Classified++
		}
	}
	return result, tx.Commit(ctx)
}

// labels each sampled prompt, prompts whose text doesn't match the synced hash are unavailable
func (s *Server) classify(ctx context.Context, scope sqlc.CurrentReviewPlanParams, b api.PromptBatch) ([]sqlc.SaveReviewSignalParams, error) {
	sequences := make([]int64, len(b.Prompts))
	for i, p := range b.Prompts {
		sequences[i] = p.Seq
	}
	events, err := s.q.ReviewPromptEvents(ctx, sqlc.ReviewPromptEventsParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, Sequences: sequences})
	if err != nil {
		return nil, err
	}
	byseq := make(map[int64]sqlc.ReviewPromptEventsRow, len(events))
	for _, e := range events {
		byseq[e.Seq] = e
	}
	type source struct {
		hour    time.Time
		session string
	}
	groups := map[string]*reviewGroup{}
	groupIDs := map[source]string{}
	var signals []sqlc.SaveReviewSignalParams
	var signalGroups []string
	for _, p := range b.Prompts {
		e, ok := byseq[p.Seq]
		if !ok {
			continue
		}
		hour := reviewHour(e.Ts.Time)
		signals = append(signals, sqlc.SaveReviewSignalParams{UserID: scope.UserID, InstallID: scope.InstallID, ProjectID: scope.ProjectID, Seq: p.Seq, EventHour: hour, Label: labelUnavailable})
		var data event.PromptSentData
		if p.Text == "" || len(p.Text) > api.MaxReviewPromptBytes || len(p.Key) != 32 ||
			json.Unmarshal(e.Data, &data) != nil || !hmac.Equal([]byte(data.Hash), []byte(prompt.Hash(p.Key, p.Text))) {
			signalGroups = append(signalGroups, "")
			continue
		}
		src := source{hour.Time, e.SessionID}
		id, ok := groupIDs[src]
		if !ok {
			id = strconv.Itoa(len(groups) + 1)
			groupIDs[src] = id
			groups[id] = &reviewGroup{}
		}
		groups[id].Prompts = append(groups[id].Prompts, reviewExcerpt(p.Text, p.Key))
		signalGroups = append(signalGroups, id)
	}
	if len(groups) == 0 {
		return signals, nil
	}
	labels, model, err := classifyReview(ctx, s.cfg.DecisionAPIKey, s.cfg.DecisionAPIURL, groups)
	if err != nil {
		return nil, err
	}
	for i, id := range signalGroups {
		if id != "" {
			signals[i].Label, signals[i].Model = labels[id], model
		}
	}
	return signals, nil
}

// long prompts become a 500 rune window, derived from the key so retries see the same text
func reviewExcerpt(text string, key []byte) string {
	const fullTextLimit, excerptRunes = 4 << 10, 500
	if len(text) <= fullTextLimit {
		return text
	}
	runes := []rune(text)
	digest := sha256.Sum256(key)
	start := binary.BigEndian.Uint64(digest[:8]) % uint64(len(runes)-excerptRunes+1)
	return string(runes[start : start+excerptRunes])
}

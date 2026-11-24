package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
	"github.com/espcaa/sablier/internal/server/db/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const reviewPerHour = 10
const monthlyReviewLimit = 300 * time.Hour

var errReviewBudget = errors.New("verification exceeds the 300-hour limit for a UTC month")

var errReviewProvider = errors.New("verifying analysis unavailable")
var errReviewPlan = errors.New("review sample changed, rerun verify to resume")

func reviewHour(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC().Truncate(time.Hour), Valid: true}
}

func lockReview(ctx context.Context, q *sqlc.Queries, user string) error {
	return q.LockAccountAnalysis(ctx, "review:"+user)
}

func (s *Server) reviewPlan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DecisionAPIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "verification model is not configured")
		return
	}
	project := r.URL.Query().Get("project")
	install, ok := verificationScope(project, r.URL.Query().Get("install"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid project or install")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	plan, err := s.prepareReview(r.Context(), apiUser(r).ID, install, project)
	if errors.Is(err, errReviewBudget) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not prepare review sample")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) prepareReview(ctx context.Context, user string, install pgtype.UUID, project string) (api.ReviewPlan, error) {
	var plan api.ReviewPlan
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return plan, err
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	if err := lockReview(ctx, q, user); err != nil {
		return plan, err
	}
	current, err := q.CurrentReviewPlan(ctx, sqlc.CurrentReviewPlanParams{UserID: user, InstallID: install, ProjectID: project})
	if err == nil {
		if err := checkReviewQuota(ctx, q, user, install, project, current.ThroughSeq, time.Now()); err != nil {
			return plan, err
		}
		return api.ReviewPlan{ThroughSeq: current.ThroughSeq, Sequences: current.Sequences}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return plan, err
	}
	after, err := q.AnalysisCheckpoint(ctx, sqlc.AnalysisCheckpointParams{UserID: user, InstallID: install, ProjectID: project})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return plan, err
	}
	rows, err := q.ReviewCandidates(ctx, sqlc.ReviewCandidatesParams{UserID: user, InstallID: install, ProjectID: project, AfterSeq: after})
	if err != nil || len(rows) == 0 {
		return plan, err
	}
	plan.ThroughSeq = rows[len(rows)-1].Seq
	if err := checkReviewQuota(ctx, q, user, install, project, plan.ThroughSeq, time.Now()); err != nil {
		return plan, err
	}
	plan.Sequences = []int64{}
	hours := make([]pgtype.Timestamptz, 0, len(rows))
	for _, row := range rows {
		hours = append(hours, reviewHour(row.Ts.Time))
	}
	used, err := reviewCounts(ctx, q, user, project, hours)
	if err != nil {
		return plan, err
	}
	rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	for _, row := range rows {
		hour := reviewHour(row.Ts.Time).Time
		if used[hour] >= reviewPerHour {
			continue
		}
		plan.Sequences = append(plan.Sequences, row.Seq)
		used[hour]++
		if len(plan.Sequences) == api.MaxReviewPrompts {
			break
		}
	}
	slices.Sort(plan.Sequences)
	err = q.CreateReviewPlan(ctx, sqlc.CreateReviewPlanParams{UserID: user, InstallID: install, ProjectID: project, ThroughSeq: plan.ThroughSeq, Sequences: plan.Sequences})
	if err != nil {
		return plan, err
	}
	return plan, tx.Commit(ctx)
}

func reviewCounts(ctx context.Context, q *sqlc.Queries, user, project string, hours []pgtype.Timestamptz) (map[time.Time]int64, error) {
	rows, err := q.ReviewHourCounts(ctx, sqlc.ReviewHourCountsParams{UserID: user, ProjectID: project, Hours: hours})
	if err != nil {
		return nil, err
	}
	counts := make(map[time.Time]int64, len(rows))
	for _, row := range rows {
		counts[row.EventHour.Time.UTC()] = row.Used
	}
	return counts, nil
}

type reviewSource struct {
	hour    time.Time
	session string
}

type reviewMetadata struct {
	Excerpt                      bool  `json:"excerpt"`
	PromptBytes                  int   `json:"prompt_bytes"`
	ComposeMs                    int64 `json:"compose_ms"`
	Pasted                       bool  `json:"pasted"`
	ApprovalLike                 bool  `json:"approval_like"`
	RepeatedInSample             bool  `json:"repeated_in_sample"`
	ShellCalls                   int64 `json:"shell_calls"`
	EditCalls                    int64 `json:"edit_calls"`
	IterationWithoutToolActivity bool  `json:"iteration_without_tool_activity"`
}

type reviewEvidence struct {
	row   sqlc.SaveReviewSignalParams
	meta  reviewMetadata
	group string
}

func (s *Server) analyzePrompts(ctx context.Context, user string, install pgtype.UUID, batch api.PromptBatch) (api.ReviewResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var result api.ReviewResult
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	q := s.q.WithTx(tx)
	if err := lockReview(ctx, q, user); err != nil {
		return result, err
	}

	scope := sqlc.AnalysisCheckpointParams{UserID: user, InstallID: install, ProjectID: batch.Project}
	after, err := q.AnalysisCheckpoint(ctx, scope)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if batch.ThroughSeq <= after {
		result.AnalyzedSeq = after
		return result, nil
	}
	plan, err := q.CurrentReviewPlan(ctx, sqlc.CurrentReviewPlanParams(scope))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, errReviewPlan
	}
	if err != nil {
		return result, err
	}
	if plan.ThroughSeq != batch.ThroughSeq || len(plan.Sequences) != len(batch.Prompts) {
		return result, errReviewPlan
	}
	for i, p := range batch.Prompts {
		if p.Seq != plan.Sequences[i] {
			return result, errReviewPlan
		}
	}
	if err := checkReviewQuota(ctx, q, user, install, batch.Project, plan.ThroughSeq, time.Now()); err != nil {
		return result, err
	}
	evidence, groups, unavailable, err := buildReview(ctx, q, user, install, batch)
	if err != nil {
		return result, err
	}
	if len(evidence) > 0 {
		labels, model, err := classifyReview(ctx, s.cfg.DecisionAPIKey, groups, s.cfg.DecisionAPIURL)
		if err != nil {
			return result, errReviewProvider
		}
		for i := range evidence {
			evidence[i].row.Label = labels[evidence[i].group]
			evidence[i].row.Model = model
			evidence[i].meta.IterationWithoutToolActivity = evidence[i].row.Label == "iteration" && evidence[i].meta.ShellCalls == 0 && evidence[i].meta.EditCalls == 0
		}
	}
	if err := q.AdvanceAnalysis(ctx, sqlc.AdvanceAnalysisParams{UserID: user, InstallID: install, ProjectID: batch.Project, LastSeq: plan.ThroughSeq, Unavailable: unavailable}); err != nil {
		return result, err
	}
	for _, e := range evidence {
		metadata, err := json.Marshal(e.meta)
		if err != nil {
			return result, err
		}
		e.row.Metadata = metadata
		if err := q.SaveReviewSignal(ctx, e.row); err != nil {
			return result, err
		}
	}
	if err := q.DropReviewPlan(ctx, sqlc.DropReviewPlanParams(scope)); err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	result.AnalyzedSeq, result.Classified, result.Unavailable = plan.ThroughSeq, len(evidence), unavailable
	return result, nil
}

func buildReview(ctx context.Context, q *sqlc.Queries, user string, install pgtype.UUID, batch api.PromptBatch) ([]reviewEvidence, map[string]*reviewGroup, int64, error) {
	if len(batch.Prompts) == 0 {
		return nil, nil, 0, nil
	}
	sequences := make([]int64, 0, len(batch.Prompts))
	repeats := map[string]int{}
	for _, p := range batch.Prompts {
		sequences = append(sequences, p.Seq)
		repeats[prompt.Normalize(p.Text)]++
	}
	events, err := q.ReviewPromptEvents(ctx, sqlc.ReviewPromptEventsParams{UserID: user, InstallID: install, ProjectID: batch.Project, Sequences: sequences})
	if err != nil {
		return nil, nil, 0, err
	}
	sources := make(map[int64]sqlc.ReviewPromptEventsRow, len(events))
	hours := make([]pgtype.Timestamptz, 0, len(events))
	for _, e := range events {
		sources[e.Seq] = e
		hours = append(hours, reviewHour(e.Ts.Time))
	}
	used, err := reviewCounts(ctx, q, user, batch.Project, hours)
	if err != nil {
		return nil, nil, 0, err
	}
	groups := map[string]*reviewGroup{}
	groupIDs := map[reviewSource]string{}
	activity := map[reviewSource]sqlc.ReviewToolActivityRow{}
	var evidence []reviewEvidence
	var unavailable int64
	var unknownSource bool
	incomplete := map[reviewSource]bool{}
	verified := map[int64]event.PromptSentData{}
	for _, p := range batch.Prompts {
		e, ok := sources[p.Seq]
		if !ok {
			unavailable++
			unknownSource = true
			continue
		}
		var data event.PromptSentData
		if !e.Hash.Valid || e.Hash.String == "" || p.Text == "" || len(p.Text) > api.MaxReviewPromptBytes || len(p.Key) != 32 ||
			json.Unmarshal(e.Data, &data) != nil || !hmac.Equal([]byte(data.Hash), []byte(prompt.Hash(p.Key, p.Text))) {
			unavailable++
			incomplete[reviewSource{reviewHour(e.Ts.Time).Time, e.SessionID}] = true
			continue
		}
		verified[p.Seq] = data
	}
	if unknownSource {
		// the missing event could belong to any group in this sample
		return nil, nil, unavailable, nil
	}
	for _, p := range batch.Prompts {
		data, ok := verified[p.Seq]
		if !ok {
			continue
		}
		e := sources[p.Seq]
		hour := reviewHour(e.Ts.Time)
		source := reviewSource{hour.Time, e.SessionID}
		if incomplete[source] {
			continue
		}
		if used[hour.Time] >= reviewPerHour {
			continue
		}
		id, ok := groupIDs[source]
		if !ok {
			id = strconv.Itoa(len(groups) + 1)
			groupIDs[source] = id
			groups[id] = &reviewGroup{}
			totals, err := q.ReviewToolActivity(ctx, sqlc.ReviewToolActivityParams{InstallID: install, ProjectID: batch.Project, SessionID: e.SessionID, HourStart: hour, HourEnd: pgtype.Timestamptz{Time: hour.Time.Add(time.Hour), Valid: true}})
			if err != nil {
				return nil, nil, 0, err
			}
			activity[source] = totals
		}
		excerpt, truncated := reviewExcerpt(p.Text, p.Key)
		groups[id].Prompts = append(groups[id].Prompts, excerpt)
		used[hour.Time]++
		totals := activity[source]
		evidence = append(evidence, reviewEvidence{
			row:   sqlc.SaveReviewSignalParams{UserID: user, InstallID: install, ProjectID: batch.Project, Seq: p.Seq, EventHour: hour},
			meta:  reviewMetadata{Excerpt: truncated, PromptBytes: len(p.Text), ComposeMs: data.ComposeMs, Pasted: data.Pasted, ApprovalLike: data.ApprovalLike, RepeatedInSample: repeats[prompt.Normalize(p.Text)] > 1, ShellCalls: totals.Shells, EditCalls: totals.Edits},
			group: id,
		})
	}
	return evidence, groups, unavailable, nil
}

// derive a stable random window so provider retries see the same evidence.
func reviewExcerpt(text string, key []byte) (string, bool) {
	const fullTextLimit = 4 << 10
	const excerptRunes = 500
	if len(text) <= fullTextLimit {
		return text, false
	}
	runes := []rune(text)
	digest := sha256.Sum256(key)
	start := binary.BigEndian.Uint64(digest[:8]) % uint64(len(runes)-excerptRunes+1)
	return string(runes[int(start) : int(start)+excerptRunes]), true
}

func (s *Server) reviewInstalls(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DecisionAPIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "verification model is not configured")
		return
	}
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
	result := make([]api.ReviewInstall, 0, len(rows))
	for _, row := range rows {
		result = append(result, api.ReviewInstall{Install: row.InstallID.String(), Seq: row.Seq, Hash: row.Hash.String})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

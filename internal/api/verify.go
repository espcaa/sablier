package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const VerifyBodyLimit = 8 << 20
const MaxReviewPrompts = 50
const MaxReviewPromptBytes = 128 << 10

type ReviewPlan struct {
	ThroughSeq int64   `json:"through_seq"`
	Sequences  []int64 `json:"sequences"`
}

type ReviewResult struct {
	AnalyzedSeq int64 `json:"analyzed_seq"`
	Unavailable int   `json:"unavailable,omitempty"`
	Classified  int   `json:"classified,omitempty"`
}

type Prompt struct {
	Key  []byte `json:"key,omitempty"`
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

type PromptBatch struct {
	ThroughSeq int64    `json:"through_seq"`
	Project    string   `json:"project"`
	Install    string   `json:"install"`
	Prompts    []Prompt `json:"prompts"`
}

// which prompts the server wants, a small sample to keep model usage low
func (c *Client) ReviewPlan(ctx context.Context, project, install string) (ReviewPlan, error) {
	query := url.Values{"project": {project}, "install": {install}}
	var result ReviewPlan
	err := c.do(ctx, http.MethodPost, "/v1/verify/plan?"+query.Encode(), nil, &result)
	return result, err
}

func (c *Client) SendPrompts(ctx context.Context, b PromptBatch) (ReviewResult, error) {
	var result ReviewResult
	body, err := json.Marshal(b)
	if err != nil {
		return result, err
	}
	if len(body) > VerifyBodyLimit {
		return result, fmt.Errorf("review sample exceeds the request size limit")
	}
	httpClient := *c.HTTP
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client := *c
	client.HTTP = &httpClient
	err = client.do(ctx, http.MethodPost, "/v1/verify", bytes.NewReader(body), &result)
	if err == nil && result.AnalyzedSeq < b.ThroughSeq {
		return result, fmt.Errorf("server did not acknowledge this batch")
	}
	return result, err
}

type ReviewInstall struct {
	Install string `json:"install"`
	Seq     int64  `json:"seq"`
	Hash    string `json:"hash"`
}

func (c *Client) ReviewInstalls(ctx context.Context, project string) ([]ReviewInstall, error) {
	query := url.Values{"project": {project}}
	var result []ReviewInstall
	err := c.do(ctx, http.MethodGet, "/v1/verify/installs?"+query.Encode(), nil, &result)
	return result, err
}

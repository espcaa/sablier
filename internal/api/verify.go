package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const VerifyBatchSize = 100
const VerifyBodyLimit = 16 << 20

type Checkpoint struct {
	LastSeq    int64      `json:"last_seq"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
}

type Prompt struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

type PromptBatch struct {
	Project string   `json:"project"`
	Install string   `json:"install"`
	Prompts []Prompt `json:"prompts"`
}

func (c *Client) Checkpoint(ctx context.Context, project, install string) (Checkpoint, error) {
	query := url.Values{"project": {project}, "install": {install}}
	var result Checkpoint
	err := c.do(ctx, http.MethodGet, "/v1/verify?"+query.Encode(), nil, &result)
	return result, err
}

// sends a prefix that fits, returning the server's checkpoint
func (c *Client) SendPrompts(ctx context.Context, b PromptBatch) (Checkpoint, error) {
	var result Checkpoint
	body, err := json.Marshal(b)
	for err == nil && len(body) > VerifyBodyLimit && len(b.Prompts) > 1 {
		b.Prompts = b.Prompts[:len(b.Prompts)/2]
		body, err = json.Marshal(b)
	}
	if err != nil {
		return result, err
	}
	if len(b.Prompts) == 0 {
		return result, fmt.Errorf("empty prompt batch")
	}
	if len(body) > VerifyBodyLimit {
		return result, fmt.Errorf("prompt %d exceeds the 16 MiB request limit", b.Prompts[0].Seq)
	}
	httpClient := *c.HTTP
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client := *c
	client.HTTP = &httpClient
	err = client.do(ctx, http.MethodPost, "/v1/verify", bytes.NewReader(body), &result)
	if err == nil && result.LastSeq < b.Prompts[len(b.Prompts)-1].Seq {
		return result, fmt.Errorf("server did not acknowledge this batch")
	}
	return result, err
}

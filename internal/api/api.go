package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/version"
)

var ErrUnauthorized = errors.New("invalid api key")

var ErrConflict = errors.New("conflict")

type Client struct {
	URL  string
	Key  string
	HTTP *http.Client
}

func New(url, key string) *Client {
	return &Client{URL: strings.TrimSuffix(url, "/"), Key: key, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, "GET", "/v1/me", nil, &u)
	return u, err
}

func (c *Client) Upload(ctx context.Context, b Batch) (Result, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return Result{}, err
	}
	var r Result
	err = c.do(ctx, "POST", "/v1/events", bytes.NewReader(body), &r)
	return r, err
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach %s: %w", c.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		if resp.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: %s", ErrConflict, e.Error)
		}
		return fmt.Errorf("server: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

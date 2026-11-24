package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

type DeviceLogin struct {
	Approval  string `json:"approval"`
	PollToken string `json:"poll_token"`
}

type DeviceCredential struct {
	Status   string `json:"status"`
	Token    string `json:"token,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

func (c *Client) StartDeviceLogin(ctx context.Context, hostname string) (DeviceLogin, error) {
	var login DeviceLogin
	err := c.deviceRequest(ctx, "/cli/start", map[string]string{"hostname": hostname}, &login)
	return login, err
}

func (c *Client) PollDeviceLogin(ctx context.Context, token string) (DeviceCredential, error) {
	var result DeviceCredential
	err := c.deviceRequest(ctx, "/cli/token", map[string]string{"poll_token": token}, &result)
	return result, err
}

func (c *Client) deviceRequest(ctx context.Context, path string, fields map[string]string, out any) error {
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	client := *c
	transport := *c.HTTP
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.HTTP = &transport
	return client.do(ctx, http.MethodPost, path, bytes.NewReader(body), out)
}

// Package control is the client side of the routing-fabric control plane: the
// daemon registers agents and sends heartbeats to the router over HTTP,
// authenticating with each agent's capability key. The server side lives in the
// private router; the request shapes are shared via botbus-proto/wire.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ericpollmann/botbus-proto/wire"
)

// Client calls the fabric control API, authenticating with a per-agent key.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient constructs a Client targeting the router control base URL
// (e.g. "http://127.0.0.1:8090").
func NewClient(base string) *Client {
	return &Client{base: base, hc: &http.Client{Timeout: 10 * time.Second}}
}

// Register idempotently writes the agent's desired state. The first call for an
// id binds the key (trust on first use); later calls must present the same key.
func (c *Client) Register(ctx context.Context, id, key string, spec wire.AgentSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/v1/agents/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, http.StatusOK)
}

// Heartbeat refreshes the agent's presence lease.
func (c *Client) Heartbeat(ctx context.Context, id, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/agents/"+id+"/heartbeat", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return c.do(req, http.StatusNoContent)
}

func (c *Client) do(req *http.Request, want int) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("control %s %s: status %d: %s", req.Method, req.URL.Path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

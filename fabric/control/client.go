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

// Deregister removes the agent's registration from the router (DELETE
// /v1/agents/{id}), authenticating with the agent's capability key. The router
// drops it from the agents set and deletes its config/filters/key, so it stops
// being a routing/classify candidate. A 204 is success; any other status is an
// error. This is the inverse of Register — the daemon/CLI calls it best-effort
// when an agent is removed locally.
func (c *Client) Deregister(ctx context.Context, id, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/v1/agents/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return c.do(req, http.StatusNoContent)
}

// Mint asks the router for a fresh, unguessable agent id. The router signs it
// with the deployment secret so it can't be forged or guessed; it's worthless
// until Register binds it to a capability key.
func (c *Client) Mint(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/mint", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("control mint: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("control mint: empty id")
	}
	return out.ID, nil
}

// Roster fetches the agent tree (GET /v1/agents), authenticating as the given
// agent (typically the root). Returns the nodes with parent links + liveness.
func (c *Client) Roster(ctx context.Context, id, key string) ([]wire.AgentNode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/agents", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Id", id)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("control roster: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var nodes []wire.AgentNode
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, err
	}
	return nodes, nil
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

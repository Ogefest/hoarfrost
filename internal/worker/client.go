package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/blocklist"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Internal API paths.
const (
	PathLease     = "/internal/lease"
	PathComplete  = "/internal/complete"
	PathDedup     = "/internal/dedup"
	PathBlocklist = "/internal/blocklist"
)

// DedupRequest is the body of a dedup lookup.
type DedupRequest struct {
	Storage string   `json:"storage,omitempty"`
	Epoch   epoch.ID `json:"epoch"`
	Digest  string   `json:"digest"`
}

// DedupResponse carries the original record, if any.
type DedupResponse struct {
	Ref *recorder.Ref `json:"ref"`
}

// LeaseResponse carries leased jobs.
type LeaseResponse struct {
	Jobs []engine.Job `json:"jobs"`
}

// HTTPCoordinator talks to a remote coordinator.
type HTTPCoordinator struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewHTTPCoordinator returns a client for the coordinator at baseURL.
func NewHTTPCoordinator(baseURL, token string) *HTTPCoordinator {
	return &HTTPCoordinator{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token, Client: &http.Client{Timeout: 2 * time.Minute}}
}

func (h *HTTPCoordinator) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return ErrEpochClosed
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("coordinator %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (h *HTTPCoordinator) Lease(ctx context.Context, r LeaseRequest) ([]engine.Job, error) {
	var resp LeaseResponse
	err := h.call(ctx, PathLease, r, &resp)
	return resp.Jobs, err
}

func (h *HTTPCoordinator) Complete(ctx context.Context, c Completion) error {
	return h.call(ctx, PathComplete, c, nil)
}

// Blocklist downloads the compiled global block lists.
func (h *HTTPCoordinator) Blocklist(ctx context.Context) (*blocklist.Set, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+PathBlocklist, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coordinator %s: %s", PathBlocklist, resp.Status)
	}
	return blocklist.Decode(resp.Body)
}

func (h *HTTPCoordinator) Lookup(ctx context.Context, storage string, ep epoch.ID, digest string) (*recorder.Ref, error) {
	var resp DedupResponse
	err := h.call(ctx, PathDedup, DedupRequest{Storage: storage, Epoch: ep, Digest: digest}, &resp)
	return resp.Ref, err
}

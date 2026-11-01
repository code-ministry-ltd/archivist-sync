// Package archive talks to The Archivist: device sign-in (spec 0005 §3), Archivist Sync's
// folders (spec 0012 §6) and the sync protocol (spec 0001 §5.5).
package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Error is an answer from the archive that isn't a success, with its code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the archive answered %d", e.Status)
}

// IsCode reports whether err is an archive error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// IsRevoked reports whether the token no longer works: revoked in the app, or unknown.
func IsRevoked(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// Client calls one archive, with a token once connected.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
	// Version goes in the User-Agent, so the archive can tell releases apart.
	Version string
}

// New makes a client with sensible timeouts.
func New(base, token, version string) *Client {
	return &Client{Base: base, Token: token, Version: version, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("content-type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("authorization", "Bearer "+c.Token)
	}
	req.Header.Set("user-agent", "archivist-sync/"+c.Version)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var wrapped struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &wrapped)
		return &Error{Status: res.StatusCode, Code: wrapped.Error.Code, Message: wrapped.Error.Message}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	return c.do(ctx, method, path, body, contentType, out)
}

// --- Device sign-in

// DeviceStart is the archive's answer to starting a sign-in.
type DeviceStart struct {
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expiresIn"`
}

// StartSignIn asks the archive to sign this computer in, one sign-in for all its folders.
func (c *Client) StartSignIn(ctx context.Context, device string) (DeviceStart, error) {
	var out DeviceStart
	err := c.json(ctx, http.MethodPost, "/api/device/start", map[string]string{"client": "sync", "device": device}, &out)
	return out, err
}

// PollSignIn returns "pending", "declined", "expired", or "approved" with the token.
func (c *Client) PollSignIn(ctx context.Context, deviceCode string) (status, token string, err error) {
	var out struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	err = c.json(ctx, http.MethodPost, "/api/device/token", map[string]string{"deviceCode": deviceCode}, &out)
	return out.Status, out.Token, err
}

// RevokeSelf revokes this computer's token: Disconnect and Uninstall.
func (c *Client) RevokeSelf(ctx context.Context) error {
	return c.json(ctx, http.MethodDelete, "/api/tokens/current", nil, nil)
}

// --- Folders (spec 0012 §6.1, §6.4)

// RootState is one folder the archive knows this computer syncs.
type RootState struct {
	Root            string `json:"root"`
	WaitingRemovals int    `json:"waitingRemovals"`
	AllowRemoval    bool   `json:"allowRemoval"`
}

// Roots lists this computer's folders, with any Remove them from the app.
func (c *Client) Roots(ctx context.Context) ([]RootState, error) {
	var out struct {
		Roots []RootState `json:"roots"`
	}
	err := c.json(ctx, http.MethodGet, "/api/sync/roots", nil, &out)
	return out.Roots, err
}

// AddRoot creates an archive folder for this computer, or continues one with existing.
func (c *Client) AddRoot(ctx context.Context, root string, existing bool) error {
	return c.json(ctx, http.MethodPost, "/api/sync/roots", map[string]any{"root": root, "existing": existing}, nil)
}

// RemoveRoot stops syncing a folder; what's archived stays.
func (c *Client) RemoveRoot(ctx context.Context, root string) error {
	return c.json(ctx, http.MethodDelete, "/api/sync/roots/"+url.PathEscape(root), nil, nil)
}

// SetWaitingRemovals tells the archive how many removals the deletion guard is holding; 0 clears.
func (c *Client) SetWaitingRemovals(ctx context.Context, root string, n int) error {
	return c.json(ctx, http.MethodPut, "/api/sync/roots/"+url.PathEscape(root), map[string]int{"waitingRemovals": n}, nil)
}

// --- The sync protocol (spec 0001 §5.5)

// Entry is one file in a manifest.
type Entry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// ManifestResult is what the archive wants after one page of a manifest.
type ManifestResult struct {
	Upload      []string `json:"upload"`
	TooLarge    []string `json:"tooLarge"`
	Unchanged   int      `json:"unchanged"`
	DeleteCount int      `json:"deleteCount"`
}

// Manifest sends one page of a full manifest.
func (c *Client) Manifest(ctx context.Context, root string, page int, last bool, entries []Entry) (ManifestResult, error) {
	var out ManifestResult
	err := c.json(ctx, http.MethodPost, "/api/sync/manifest", map[string]any{
		"root": root, "source": "note", "full": true, "page": page, "last": last, "entries": entries,
	}, &out)
	return out, err
}

// Upload sends one file's bytes.
func (c *Client) Upload(ctx context.Context, root, path string, data []byte) error {
	q := url.Values{"root": {root}, "path": {path}}
	return c.do(ctx, http.MethodPut, "/api/sync/files?"+q.Encode(), bytes.NewReader(data), "application/octet-stream", nil)
}

// Commit starts the import of what was sent; it returns the import's ID.
func (c *Client) Commit(ctx context.Context, root string) (string, error) {
	var out struct {
		ImportID string `json:"importId"`
	}
	err := c.json(ctx, http.MethodPost, "/api/sync/commit", map[string]string{"root": root}, &out)
	return out.ImportID, err
}

package studioruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// maxResponse bounds every API response the runtime reads.
const maxResponse = 4 << 20

// apiError is a non-2xx answer: its status and error code only. The body
// is never logged; it may carry an action's payload.
type apiError struct {
	Status     int
	Code       string
	Reason     string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("api: %d %s", e.Status, e.Code)
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// client calls the EACP API with one key per request. It never follows a
// redirect, so a key is sent only to the configured origin.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{
		Timeout:       2 * maxWaitSeconds * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends in (JSON, when not nil) as key and decodes a 2xx body into out.
func (c *client) do(ctx context.Context, key, method, path string, header map[string]string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("api %s %s: read: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{Status: resp.StatusCode}
		var e struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(raw, &e) == nil {
			ae.Code, ae.Reason = e.Error, e.Reason
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		return resp.StatusCode, ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("api %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

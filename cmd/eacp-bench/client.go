package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// client calls the API as named key holders. Keys stay in this map, in
// memory: they are never logged, and error messages never include them.
type client struct {
	base string
	http *http.Client

	mu   sync.Mutex
	keys map[string]string
}

func newClient(base string) *client {
	t := &http.Transport{MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 90 * time.Second}
	return &client{base: base, http: &http.Client{Transport: t, Timeout: 60 * time.Second}, keys: map[string]string{}}
}

func (c *client) setKey(who, key string) {
	c.mu.Lock()
	c.keys[who] = key
	c.mu.Unlock()
}

func (c *client) key(who string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys[who]
}

// secrets returns every key held, for the results writer's refusal check.
func (c *client) secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.keys))
	for _, k := range c.keys {
		out = append(out, k)
	}
	return out
}

// call sends one request as who and decodes a JSON object reply.
func (c *client) call(ctx context.Context, who, method, path string, body any) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key(who))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, nil
}

// must is call that requires status want.
func (c *client) must(ctx context.Context, want int, who, method, path string, body any) (map[string]any, error) {
	code, out, err := c.call(ctx, who, method, path, body)
	if err != nil {
		return nil, err
	}
	if code != want {
		return nil, fmt.Errorf("%s %s as %s = %d %v, want %d", method, path, who, code, out, want)
	}
	return out, nil
}

// id returns the reply's "id".
func id(out map[string]any) string {
	s, _ := out["id"].(string)
	return s
}

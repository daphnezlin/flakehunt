package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// toxiproxyClient talks to toxiproxy's http api (default port 8474).
// Only the three calls we need, so no extra dependency for a client library.
type toxiproxyClient struct {
	base string // e.g. http://127.0.0.1:55012
	http *http.Client
}

func newToxiproxyClient(base string) toxiproxyClient {
	return toxiproxyClient{base: base, http: &http.Client{Timeout: 5 * time.Second}}
}

// waitReady polls until the api answers, since the container takes a moment to start.
func (c toxiproxyClient) waitReady(ctx context.Context) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/version", nil)
		resp, err := c.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("toxiproxy never became ready: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c toxiproxyClient) createProxy(ctx context.Context, name, listen, upstream string) error {
	return c.post(ctx, "/proxies", map[string]any{
		"name": name, "listen": listen, "upstream": upstream, "enabled": true,
	})
}

// addLatency delays data coming back from the service (downstream) by
// latency +/- jitter ms, so every response is slow but by a varying amount.
func (c toxiproxyClient) addLatency(ctx context.Context, proxy string, latencyMS, jitterMS int) error {
	return c.post(ctx, "/proxies/"+proxy+"/toxics", map[string]any{
		"name": "latency", "type": "latency", "stream": "downstream", "toxicity": 1.0,
		"attributes": map[string]int{"latency": latencyMS, "jitter": jitterMS},
	})
}

func (c toxiproxyClient) post(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("toxiproxy %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("toxiproxy %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

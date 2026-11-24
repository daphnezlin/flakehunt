package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fake toxiproxy api that records what it was sent
func fakeToxiproxy(t *testing.T) (*httptest.Server, *[]map[string]any, *[]string) {
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/version" {
			w.Write([]byte(`{"version":"2.13.0"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad json to %s: %v", r.URL.Path, err)
		}
		bodies = append(bodies, body)
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/proxies" {
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &paths
}

func TestToxiproxyClient(t *testing.T) {
	srv, bodies, paths := fakeToxiproxy(t)
	c := newToxiproxyClient(srv.URL)
	ctx := context.Background()

	if err := c.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.createProxy(ctx, "service", "0.0.0.0:8666", "service:8000"); err != nil {
		t.Fatal(err)
	}
	if err := c.addLatency(ctx, "service", 150, 75); err != nil {
		t.Fatal(err)
	}

	if (*paths)[0] != "/proxies" || (*bodies)[0]["upstream"] != "service:8000" || (*bodies)[0]["enabled"] != true {
		t.Errorf("create proxy: %s %v", (*paths)[0], (*bodies)[0])
	}
	toxic := (*bodies)[1]
	attrs := toxic["attributes"].(map[string]any)
	if (*paths)[1] != "/proxies/service/toxics" || toxic["type"] != "latency" || toxic["stream"] != "downstream" ||
		attrs["latency"] != 150.0 || attrs["jitter"] != 75.0 {
		t.Errorf("add toxic: %s %v", (*paths)[1], toxic)
	}
}

func TestToxiproxyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"proxy already exists"}`, http.StatusConflict)
	}))
	defer srv.Close()
	err := newToxiproxyClient(srv.URL).createProxy(context.Background(), "a", "b", "c")
	if err == nil {
		t.Fatal("expected error on 409")
	}
}

func TestToxiproxyWaitTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := newToxiproxyClient("http://127.0.0.1:1").waitReady(ctx); err == nil {
		t.Fatal("expected timeout")
	}
}

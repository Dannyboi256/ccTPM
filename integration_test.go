package main

import (
	"bytes"
	"claude-proxy/proxy"
	"claude-proxy/store"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyForwardsAndCapturesTokens(t *testing.T) {
	// Mock upstream returns JSON with usage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{
			"id": "msg_123",
			"type": "message",
			"model": "claude-sonnet-4-20250514",
			"usage": {
				"input_tokens": 1000,
				"output_tokens": 500,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens": 200
			},
			"content": [{"type": "text", "text": "Hello"}]
		}`)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)
	go func() {
		for range dbChan {
		}
	}()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: "test",
		Store:       st,
		DBChan:      dbChan,
	})

	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model": "claude-sonnet-4-20250514", "messages": []}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Hello") {
		t.Fatalf("response body not forwarded")
	}

	time.Sleep(100 * time.Millisecond) // Wait for parser goroutine

	sess := st.GetSession("test")
	if sess == nil {
		t.Fatal("no session found")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(sess.Requests))
	}
	r := sess.Requests[0]
	if r.InputTokens != 1000 {
		t.Fatalf("expected 1000 input, got %d", r.InputTokens)
	}
	if r.OutputTokens != 500 {
		t.Fatalf("expected 500 output, got %d", r.OutputTokens)
	}
	if r.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("wrong model: %s", r.Model)
	}

	close(dbChan)
}

func TestProxyHandlesSSEStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":2000,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":500}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":300}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, e := range events {
			fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)
	go func() {
		for range dbChan {
		}
	}()

	p := proxy.NewProxy(proxy.Config{UpstreamURL: upstreamURL, SessionName: "sse-test", Store: st, DBChan: dbChan})
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, _ := http.Post(proxyServer.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	io.ReadAll(resp.Body)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	sess := st.GetSession("sse-test")
	if sess == nil {
		t.Fatal("no session")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1, got %d", len(sess.Requests))
	}
	r := sess.Requests[0]
	if r.InputTokens != 2000 {
		t.Fatalf("expected 2000 input, got %d", r.InputTokens)
	}
	if r.OutputTokens != 300 {
		t.Fatalf("expected 300 output, got %d", r.OutputTokens)
	}
	if r.CacheRead != 500 {
		t.Fatalf("expected 500 cache read, got %d", r.CacheRead)
	}

	close(dbChan)
}

func TestProxyHandles429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error": {"type": "rate_limit_error"}}`)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)
	go func() {
		for range dbChan {
		}
	}()

	p := proxy.NewProxy(proxy.Config{UpstreamURL: upstreamURL, SessionName: "throttle-test", Store: st, DBChan: dbChan})
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, _ := http.Post(proxyServer.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)

	sess := st.GetSession("throttle-test")
	if sess == nil {
		t.Fatal("no session")
	}
	r := sess.Requests[0]
	if r.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", r.StatusCode)
	}
	if r.RetryAfter != "30" {
		t.Fatalf("expected retry '30', got '%s'", r.RetryAfter)
	}

	close(dbChan)
}

func TestProxyHandlesGzipSSEStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)

		events := strings.Join([]string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":4000,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":1000}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":600}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}, "")

		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte(events))
		gz.Close()
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)
	go func() { for range dbChan {} }()

	p := proxy.NewProxy(proxy.Config{UpstreamURL: upstreamURL, SessionName: "gzip-test", Store: st, DBChan: dbChan})
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	sess := st.GetSession("gzip-test")
	if sess == nil {
		t.Fatal("no session")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(sess.Requests))
	}
	r := sess.Requests[0]
	if r.InputTokens != 4000 {
		t.Fatalf("expected 4000 input, got %d", r.InputTokens)
	}
	if r.OutputTokens != 600 {
		t.Fatalf("expected 600 output, got %d", r.OutputTokens)
	}
	if r.CacheRead != 1000 {
		t.Fatalf("expected 1000 cache read, got %d", r.CacheRead)
	}
	if r.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %q", r.Model)
	}

	close(dbChan)
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"0d", 0, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"d", 0, true},
		{"xd", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.input, err)
			}
			if got != tt.expected {
				t.Fatalf("parseDuration(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

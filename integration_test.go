package main

import (
	"bytes"
	"claude-proxy/db"
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

func waitForSession(t *testing.T, st *store.Store, sessionID string, timeout time.Duration) *store.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess := st.GetSession(sessionID); sess != nil && len(sess.Requests) > 0 {
			return sess
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session %q", sessionID)
	return nil
}

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

	sess := waitForSession(t, st, "test", 2*time.Second)
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

	sess := waitForSession(t, st, "sse-test", 2*time.Second)
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

	sess := waitForSession(t, st, "throttle-test", 2*time.Second)
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

	sess := waitForSession(t, st, "gzip-test", 2*time.Second)
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

// setupProxy returns a store, dbChan, db, and httptest proxy server wired to the given upstream.
func setupProxy(t *testing.T, upstreamURL string) (*store.Store, chan store.RequestRecord, *db.DB, *httptest.Server) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 64)
	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: u,
		SessionName: "default",
		Store:       st,
		DBChan:      dbChan,
	})
	srv := httptest.NewServer(p)
	return st, dbChan, d, srv
}

func TestIntegration_ProxyCapturesAnthropicRateLimitHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("anthropic-ratelimit-input-tokens-limit", "450000")
		w.Header().Set("anthropic-ratelimit-input-tokens-remaining", "448500")
		w.Header().Set("anthropic-ratelimit-output-tokens-limit", "90000")
		w.Header().Set("anthropic-ratelimit-output-tokens-remaining", "89200")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "998")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":20}}`))
	}))
	defer upstream.Close()

	st, dbChan, d, proxyServer := setupProxy(t, upstream.URL)
	defer proxyServer.Close()
	defer d.Close()
	_ = st

	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Drain the DB channel for the record
	var rec store.RequestRecord
	select {
	case rec = <-dbChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for record")
	}
	if rec.ITokensLimit == nil || *rec.ITokensLimit != 450000 {
		t.Errorf("ITokensLimit: expected 450000, got %v", rec.ITokensLimit)
	}
	if rec.OTokensRemaining == nil || *rec.OTokensRemaining != 89200 {
		t.Errorf("OTokensRemaining: expected 89200, got %v", rec.OTokensRemaining)
	}

	// Insert through the db and read back to confirm round-trip
	if err := d.InsertRecord(rec); err != nil {
		t.Fatal(err)
	}
	rows, err := d.QueryRequests("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ITokensLimit == nil || *rows[0].ITokensLimit != 450000 {
		t.Fatal("DB round-trip lost ITokensLimit")
	}
}

func TestIntegration_ProxyCapturesUnifiedOAuthHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.0184169696")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "1712345678")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.7370692663")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4","usage":{"input_tokens":50,"output_tokens":10}}`))
	}))
	defer upstream.Close()

	_, dbChan, d, proxyServer := setupProxy(t, upstream.URL)
	defer proxyServer.Close()
	defer d.Close()

	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var rec store.RequestRecord
	select {
	case rec = <-dbChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for record")
	}
	if rec.Unified5hUtil == nil || *rec.Unified5hUtil < 0.018 || *rec.Unified5hUtil > 0.019 {
		t.Errorf("Unified5hUtil wrong: %v", rec.Unified5hUtil)
	}
	if rec.UnifiedStatus != "allowed" {
		t.Errorf("UnifiedStatus wrong: %q", rec.UnifiedStatus)
	}
	if rec.UnifiedReprClaim != "five_hour" {
		t.Errorf("UnifiedReprClaim wrong: %q", rec.UnifiedReprClaim)
	}
}

func TestIntegration_EndToEndTPMMeasurement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4","usage":{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":50}}`))
	}))
	defer upstream.Close()

	st, dbChan, d, proxyServer := setupProxy(t, upstream.URL)
	defer proxyServer.Close()
	defer d.Close()

	// Fire 5 requests
	for i := 0; i < 5; i++ {
		resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Collect records and insert into DB
	for i := 0; i < 5; i++ {
		select {
		case rec := <-dbChan:
			if err := d.InsertRecord(rec); err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for record %d", i)
		}
	}

	// In-memory rolling metrics
	now := time.Now()
	itpm := st.RollingITPM("default", now)
	if itpm < 5*1050 { // 5 * (1000 input + 50 cache_creation)
		t.Errorf("in-memory ITPM too low: got %v, want at least %v", itpm, 5*1050)
	}

	// DB TPM bucket should show at least one bucket with the same total
	buckets, err := d.QueryTPMBuckets(now.Add(-2*time.Minute), now.Add(1*time.Minute), 60, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) == 0 {
		t.Fatal("expected at least one bucket, got none")
	}
	var sumITPM float64
	for _, b := range buckets {
		sumITPM += b.MaxITPM
	}
	if sumITPM < 5*1050 {
		t.Errorf("DB TPM buckets too low: sum %v, want at least %v", sumITPM, 5*1050)
	}
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

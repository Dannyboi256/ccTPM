package proxy

import (
	"bytes"
	"claude-proxy/store"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// makeResp returns a minimal *http.Response with the given headers and no body.
func makeResp(headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: 200,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    &http.Request{Header: http.Header{}},
	}
}

func TestExtractRateLimitHeaders_AnthropicPerDirection(t *testing.T) {
	resp := makeResp(map[string]string{
		"anthropic-ratelimit-input-tokens-limit":     "450000",
		"anthropic-ratelimit-input-tokens-remaining": "448500",
		"anthropic-ratelimit-input-tokens-reset":     "2026-04-05T00:00:00Z",
		"anthropic-ratelimit-output-tokens-limit":     "16000",
		"anthropic-ratelimit-output-tokens-remaining": "15000",
		"anthropic-ratelimit-output-tokens-reset":     "2026-04-05T00:01:00Z",
		"anthropic-ratelimit-requests-limit":          "1000",
		"anthropic-ratelimit-requests-remaining":      "998",
		"anthropic-ratelimit-requests-reset":          "2026-04-05T00:02:00Z",
		"anthropic-ratelimit-tokens-remaining":        "9999",
	})

	rl := extractRateLimitHeaders(resp)

	if rl.ITokensLimit == nil || *rl.ITokensLimit != 450000 {
		t.Fatalf("ITokensLimit: got %v", rl.ITokensLimit)
	}
	if rl.ITokensRemaining == nil || *rl.ITokensRemaining != 448500 {
		t.Fatalf("ITokensRemaining: got %v", rl.ITokensRemaining)
	}
	if rl.ITokensReset != "2026-04-05T00:00:00Z" {
		t.Fatalf("ITokensReset: got %q", rl.ITokensReset)
	}
	if rl.OTokensLimit == nil || *rl.OTokensLimit != 16000 {
		t.Fatalf("OTokensLimit: got %v", rl.OTokensLimit)
	}
	if rl.OTokensRemaining == nil || *rl.OTokensRemaining != 15000 {
		t.Fatalf("OTokensRemaining: got %v", rl.OTokensRemaining)
	}
	if rl.RPMLimit == nil || *rl.RPMLimit != 1000 {
		t.Fatalf("RPMLimit: got %v", rl.RPMLimit)
	}
	if rl.RPMRemaining == nil || *rl.RPMRemaining != 998 {
		t.Fatalf("RPMRemaining: got %v", rl.RPMRemaining)
	}
	if rl.LegacyTokensRemaining == nil || *rl.LegacyTokensRemaining != 9999 {
		t.Fatalf("LegacyTokensRemaining: got %v", rl.LegacyTokensRemaining)
	}
}

func TestExtractRateLimitHeaders_UnifiedOAuth(t *testing.T) {
	resp := makeResp(map[string]string{
		"anthropic-ratelimit-unified-status":               "allowed",
		"anthropic-ratelimit-unified-5h-utilization":       "0.0184",
		"anthropic-ratelimit-unified-5h-reset":             "1743810000",
		"anthropic-ratelimit-unified-7d-utilization":       "0.0042",
		"anthropic-ratelimit-unified-7d-reset":             "1744200000",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
	})

	rl := extractRateLimitHeaders(resp)

	if rl.UnifiedStatus != "allowed" {
		t.Fatalf("UnifiedStatus: got %q", rl.UnifiedStatus)
	}
	if rl.Unified5hUtil == nil || *rl.Unified5hUtil < 0.018 || *rl.Unified5hUtil > 0.019 {
		t.Fatalf("Unified5hUtil: got %v", rl.Unified5hUtil)
	}
	if rl.Unified5hReset == nil || *rl.Unified5hReset != 1743810000 {
		t.Fatalf("Unified5hReset: got %v", rl.Unified5hReset)
	}
	if rl.Unified7dUtil == nil || *rl.Unified7dUtil < 0.004 || *rl.Unified7dUtil > 0.005 {
		t.Fatalf("Unified7dUtil: got %v", rl.Unified7dUtil)
	}
	if rl.Unified7dReset == nil || *rl.Unified7dReset != 1744200000 {
		t.Fatalf("Unified7dReset: got %v", rl.Unified7dReset)
	}
	if rl.UnifiedReprClaim != "five_hour" {
		t.Fatalf("UnifiedReprClaim: got %q", rl.UnifiedReprClaim)
	}
}

func TestExtractRateLimitHeaders_BothSetsCoexist(t *testing.T) {
	resp := makeResp(map[string]string{
		"anthropic-ratelimit-input-tokens-limit":     "450000",
		"anthropic-ratelimit-input-tokens-remaining": "440000",
		"anthropic-ratelimit-unified-status":         "rate_limited",
		"anthropic-ratelimit-unified-5h-utilization": "0.99",
	})

	rl := extractRateLimitHeaders(resp)

	if rl.ITokensLimit == nil || *rl.ITokensLimit != 450000 {
		t.Fatalf("ITokensLimit: got %v", rl.ITokensLimit)
	}
	if rl.ITokensRemaining == nil || *rl.ITokensRemaining != 440000 {
		t.Fatalf("ITokensRemaining: got %v", rl.ITokensRemaining)
	}
	if rl.UnifiedStatus != "rate_limited" {
		t.Fatalf("UnifiedStatus: got %q", rl.UnifiedStatus)
	}
	if rl.Unified5hUtil == nil || *rl.Unified5hUtil < 0.98 {
		t.Fatalf("Unified5hUtil: got %v", rl.Unified5hUtil)
	}
}

func TestExtractRateLimitHeaders_AllAbsent(t *testing.T) {
	resp := makeResp(map[string]string{})

	rl := extractRateLimitHeaders(resp)

	if rl.ITokensLimit != nil {
		t.Fatalf("ITokensLimit should be nil, got %v", rl.ITokensLimit)
	}
	if rl.ITokensRemaining != nil {
		t.Fatalf("ITokensRemaining should be nil, got %v", rl.ITokensRemaining)
	}
	if rl.OTokensLimit != nil {
		t.Fatalf("OTokensLimit should be nil, got %v", rl.OTokensLimit)
	}
	if rl.RPMLimit != nil {
		t.Fatalf("RPMLimit should be nil, got %v", rl.RPMLimit)
	}
	if rl.RPMRemaining != nil {
		t.Fatalf("RPMRemaining should be nil, got %v", rl.RPMRemaining)
	}
	if rl.LegacyTokensRemaining != nil {
		t.Fatalf("LegacyTokensRemaining should be nil, got %v", rl.LegacyTokensRemaining)
	}
	if rl.Unified5hUtil != nil {
		t.Fatalf("Unified5hUtil should be nil, got %v", rl.Unified5hUtil)
	}
	if rl.Unified5hReset != nil {
		t.Fatalf("Unified5hReset should be nil, got %v", rl.Unified5hReset)
	}
	if rl.UnifiedStatus != "" {
		t.Fatalf("UnifiedStatus should be empty, got %q", rl.UnifiedStatus)
	}
	if rl.UnifiedReprClaim != "" {
		t.Fatalf("UnifiedReprClaim should be empty, got %q", rl.UnifiedReprClaim)
	}
	if rl.RetryAfter != "" {
		t.Fatalf("RetryAfter should be empty, got %q", rl.RetryAfter)
	}
}

func TestExtractRateLimitHeaders_MalformedUtilization(t *testing.T) {
	resp := makeResp(map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "not-a-float",
		"anthropic-ratelimit-unified-5h-reset":       "not-an-int",
		"anthropic-ratelimit-input-tokens-limit":     "also-bad",
	})

	rl := extractRateLimitHeaders(resp)

	if rl.Unified5hUtil != nil {
		t.Fatalf("Unified5hUtil should be nil for malformed value, got %v", rl.Unified5hUtil)
	}
	if rl.Unified5hReset != nil {
		t.Fatalf("Unified5hReset should be nil for malformed value, got %v", rl.Unified5hReset)
	}
	if rl.ITokensLimit != nil {
		t.Fatalf("ITokensLimit should be nil for malformed value, got %v", rl.ITokensLimit)
	}
}

func TestExtractRateLimitHeaders_UnifiedResetAsRFC3339(t *testing.T) {
	// The API might return RFC3339 instead of unix seconds; parseUnixOrRFC3339 should handle it.
	resp := makeResp(map[string]string{
		"anthropic-ratelimit-unified-5h-reset": "2026-04-05T00:00:00Z",
	})

	rl := extractRateLimitHeaders(resp)

	if rl.Unified5hReset == nil {
		t.Fatal("Unified5hReset should not be nil for RFC3339 value")
	}
	// 2026-04-05T00:00:00Z unix = 1775347200
	if *rl.Unified5hReset != 1775347200 {
		t.Fatalf("Unified5hReset: got %d, want 1775347200", *rl.Unified5hReset)
	}
}

func TestTeeReadCloserForwardsData(t *testing.T) {
	original := io.NopCloser(bytes.NewReader([]byte("hello world")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, time.Now())

	buf := make([]byte, 11)
	n, err := trc.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(buf[:n]))
	}

	trc.Close()

	// Verify data was sent to channel
	var received []byte
	for chunk := range ch {
		received = append(received, chunk...)
	}
	if string(received) != "hello world" {
		t.Fatalf("channel received '%s', expected 'hello world'", string(received))
	}
}

func TestTeeReadCloserTTFT(t *testing.T) {
	start := time.Now()
	original := io.NopCloser(bytes.NewReader([]byte("data")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, start)

	time.Sleep(10 * time.Millisecond)
	buf := make([]byte, 4)
	trc.Read(buf)

	ttft := trc.TTFT()
	if ttft < 10*time.Millisecond {
		t.Fatalf("TTFT too short: %v", ttft)
	}

	trc.Close()
}

func TestTeeReadCloserCloseSignalsChannel(t *testing.T) {
	original := io.NopCloser(bytes.NewReader([]byte("x")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, time.Now())

	buf := make([]byte, 1)
	trc.Read(buf)
	trc.Close()

	select {
	case _, ok := <-ch:
		if ok {
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after Close()")
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		fallback string
		want     string
	}{
		{"x-session-id", http.Header{"X-Session-Id": {"abc"}}, "default", "abc"},
		{"anthropic-session-id", http.Header{"Anthropic-Session-Id": {"def"}}, "default", "def"},
		{"fallback", http.Header{}, "my-session", "my-session"},
		{"x-session-id takes priority", http.Header{
			"X-Session-Id":         {"first"},
			"Anthropic-Session-Id": {"second"},
		}, "default", "first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSessionID(tt.headers, tt.fallback)
			if got != tt.want {
				t.Fatalf("expected '%s', got '%s'", tt.want, got)
			}
		})
	}
}

func TestModifyResponsePopulatesAllHeaderFields(t *testing.T) {
	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 16)
	cfg := Config{
		UpstreamURL: &url.URL{Scheme: "https", Host: "api.anthropic.com"},
		SessionName: "test-session",
		Store:       st,
		DBChan:      dbChan,
	}

	body := `{"id":"msg_1","type":"message","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":50}}`
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Request:    &http.Request{URL: &url.URL{Path: "/v1/messages"}, Header: http.Header{}},
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("anthropic-ratelimit-input-tokens-limit", "450000")
	resp.Header.Set("anthropic-ratelimit-input-tokens-remaining", "448500")
	resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "0.0184")
	resp.Header.Set("anthropic-ratelimit-unified-status", "allowed")
	resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), requestStartKey, time.Now()))

	if err := modifyResponse(resp, cfg); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	var rec store.RequestRecord
	select {
	case rec = <-dbChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for DB record")
	}

	if rec.ITokensLimit == nil || *rec.ITokensLimit != 450000 {
		t.Fatalf("ITokensLimit not populated: %v", rec.ITokensLimit)
	}
	if rec.Unified5hUtil == nil || *rec.Unified5hUtil < 0.018 {
		t.Fatalf("Unified5hUtil not populated: %v", rec.Unified5hUtil)
	}
	if rec.UnifiedStatus != "allowed" {
		t.Fatalf("UnifiedStatus not populated: %q", rec.UnifiedStatus)
	}
}

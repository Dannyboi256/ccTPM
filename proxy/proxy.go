package proxy

import (
	"claude-proxy/parser"
	"claude-proxy/store"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type contextKey string

const requestStartKey contextKey = "requestStart"

type teeReadCloser struct {
	original         io.ReadCloser
	ch               chan []byte
	requestStartTime time.Time
	ttftNs           atomic.Int64
	closeOnce        sync.Once
}

func newTeeReadCloser(original io.ReadCloser, ch chan []byte, requestStart time.Time) *teeReadCloser {
	return &teeReadCloser{
		original:         original,
		ch:               ch,
		requestStartTime: requestStart,
	}
}

func (r *teeReadCloser) Read(p []byte) (int, error) {
	n, err := r.original.Read(p)
	if n > 0 {
		r.ttftNs.CompareAndSwap(0, int64(time.Since(r.requestStartTime)))
		cp := make([]byte, n)
		copy(cp, p[:n])
		r.ch <- cp
	}
	if err != nil {
		r.closeChannel()
	}
	return n, err
}

func (r *teeReadCloser) Close() error {
	r.closeChannel()
	return r.original.Close()
}

func (r *teeReadCloser) closeChannel() {
	r.closeOnce.Do(func() {
		close(r.ch)
	})
}

func (r *teeReadCloser) TTFT() time.Duration {
	return time.Duration(r.ttftNs.Load())
}

func ExtractSessionID(headers http.Header, fallback string) string {
	if v := headers.Get("X-Session-Id"); v != "" {
		return v
	}
	if v := headers.Get("Anthropic-Session-Id"); v != "" {
		return v
	}
	return fallback
}

// RateLimitHeaders holds all rate-limit-related response headers from the Anthropic API.
type RateLimitHeaders struct {
	ITokensLimit     *int
	ITokensRemaining *int
	ITokensReset     string
	OTokensLimit     *int
	OTokensRemaining *int
	OTokensReset     string
	RPMLimit         *int
	RPMRemaining     *int
	RPMReset         string

	// Legacy combined tokens header
	LegacyTokensRemaining *int

	// Unified OAuth headers
	Unified5hUtil    *float64
	Unified5hReset   *int64
	Unified5hStatus  string
	Unified7dUtil    *float64
	Unified7dReset   *int64
	Unified7dStatus  string
	UnifiedStatus    string
	UnifiedReprClaim string

	RetryAfter string
}

func parseIntHeader(h http.Header, name string) *int {
	v := h.Get(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func parseFloatHeader(h http.Header, name string) *float64 {
	v := h.Get(name)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

func parseUnixOrRFC3339(h http.Header, name string) *int64 {
	v := h.Get(name)
	if v == "" {
		return nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return &n
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		sec := t.Unix()
		return &sec
	}
	return nil
}

func extractRateLimitHeaders(resp *http.Response) RateLimitHeaders {
	h := resp.Header
	var out RateLimitHeaders

	out.RetryAfter = h.Get("Retry-After")

	// Per-direction input token headers
	out.ITokensLimit = parseIntHeader(h, "anthropic-ratelimit-input-tokens-limit")
	out.ITokensRemaining = parseIntHeader(h, "anthropic-ratelimit-input-tokens-remaining")
	out.ITokensReset = h.Get("anthropic-ratelimit-input-tokens-reset")

	// Per-direction output token headers
	out.OTokensLimit = parseIntHeader(h, "anthropic-ratelimit-output-tokens-limit")
	out.OTokensRemaining = parseIntHeader(h, "anthropic-ratelimit-output-tokens-remaining")
	out.OTokensReset = h.Get("anthropic-ratelimit-output-tokens-reset")

	// Requests (RPM) headers
	out.RPMLimit = parseIntHeader(h, "anthropic-ratelimit-requests-limit")
	out.RPMRemaining = parseIntHeader(h, "anthropic-ratelimit-requests-remaining")
	out.RPMReset = h.Get("anthropic-ratelimit-requests-reset")

	// Legacy combined tokens header — distinct from ITokensRemaining, do not conflate
	out.LegacyTokensRemaining = parseIntHeader(h, "anthropic-ratelimit-tokens-remaining")

	// Unified OAuth headers
	out.Unified5hUtil = parseFloatHeader(h, "anthropic-ratelimit-unified-5h-utilization")
	out.Unified5hReset = parseUnixOrRFC3339(h, "anthropic-ratelimit-unified-5h-reset")
	out.Unified5hStatus = h.Get("anthropic-ratelimit-unified-5h-status")
	out.Unified7dUtil = parseFloatHeader(h, "anthropic-ratelimit-unified-7d-utilization")
	out.Unified7dReset = parseUnixOrRFC3339(h, "anthropic-ratelimit-unified-7d-reset")
	out.Unified7dStatus = h.Get("anthropic-ratelimit-unified-7d-status")
	out.UnifiedStatus = h.Get("anthropic-ratelimit-unified-status")
	out.UnifiedReprClaim = h.Get("anthropic-ratelimit-unified-representative-claim")

	return out
}

type Config struct {
	UpstreamURL *url.URL
	SessionName string
	Store       *store.Store
	DBChan      chan<- store.RequestRecord
}

func NewProxy(cfg Config) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.UpstreamURL)
			pr.Out.Host = cfg.UpstreamURL.Host
			// Restrict to gzip only — we decompress in the parser goroutine.
			// Prevents brotli/deflate responses we can't decode.
			pr.Out.Header.Set("Accept-Encoding", "gzip")
			ctx := context.WithValue(pr.Out.Context(), requestStartKey, time.Now())
			pr.Out = pr.Out.WithContext(ctx)
		},
		ModifyResponse: func(resp *http.Response) error {
			return modifyResponse(resp, cfg)
		},
	}
}

func modifyResponse(resp *http.Response, cfg Config) error {
	req := resp.Request
	sessionID := ExtractSessionID(req.Header, cfg.SessionName)
	endpoint := req.URL.Path
	requestStart, _ := req.Context().Value(requestStartKey).(time.Time)
	if requestStart.IsZero() {
		requestStart = time.Now()
	}
	rateHeaders := extractRateLimitHeaders(resp)
	retryAfter := rateHeaders.RetryAfter
	reqRemaining := -1
	if rateHeaders.RPMRemaining != nil {
		reqRemaining = *rateHeaders.RPMRemaining
	}
	tokRemaining := -1
	if rateHeaders.LegacyTokensRemaining != nil {
		tokRemaining = *rateHeaders.LegacyTokensRemaining
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rec := store.RequestRecord{
			StartTime:             requestStart,
			EndTime:               time.Now(),
			Endpoint:              endpoint,
			StatusCode:            resp.StatusCode,
			RetryAfter:            retryAfter,
			RateLimitReqRemaining: reqRemaining,
			RateLimitTokRemaining: tokRemaining,
			SessionID:             sessionID,
		}
		cfg.Store.AddRecord(rec)
		select {
		case cfg.DBChan <- rec:
		default:
			log.Println("warning: DB write channel full, record dropped")
		}
		return nil
	}

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	isGzip := strings.Contains(resp.Header.Get("Content-Encoding"), "gzip")
	inflightID := cfg.Store.AddInFlight(sessionID, endpoint)

	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(resp.Body, ch, requestStart)
	resp.Body = trc

	go func() {
		defer cfg.Store.RemoveInFlight(inflightID)

		var parserInput io.Reader = newChannelReader(ch)
		if isGzip {
			gz, err := gzip.NewReader(parserInput)
			if err != nil {
				log.Printf("gzip reader init: %v", err)
				// drain channel so tee-reader never blocks
				for range ch {
				}
				rec := store.RequestRecord{
					StartTime:             requestStart,
					EndTime:               time.Now(),
					Endpoint:              endpoint,
					StatusCode:            resp.StatusCode,
					RetryAfter:            retryAfter,
					RateLimitReqRemaining: reqRemaining,
					RateLimitTokRemaining: tokRemaining,
					HasError:              true,
					SessionID:             sessionID,
				}
				cfg.Store.AddRecord(rec)
				cfg.DBChan <- rec
				return
			}
			defer gz.Close()
			parserInput = gz
		}

		var result parser.Result
		if isSSE {
			result, _ = parser.ParseSSE(parserInput)
		} else {
			var buf []byte
			if isGzip {
				buf, _ = io.ReadAll(parserInput)
			} else {
				for chunk := range ch {
					buf = append(buf, chunk...)
				}
			}
			result, _ = parser.ParseJSON(buf)
		}
		endTime := time.Now()

		rec := store.RequestRecord{
			StartTime:             requestStart,
			EndTime:               endTime,
			Model:                 result.Model,
			InputTokens:           result.InputTokens,
			OutputTokens:          result.OutputTokens,
			CacheCreation:         result.CacheCreation,
			CacheRead:             result.CacheRead,
			Endpoint:              endpoint,
			StatusCode:            resp.StatusCode,
			TTFT:                  trc.TTFT(),
			RetryAfter:            retryAfter,
			RateLimitReqRemaining: reqRemaining,
			RateLimitTokRemaining: tokRemaining,
			HasError:              result.HasError,
			SessionID:             sessionID,
		}
		cfg.Store.AddRecord(rec)
		select {
		case cfg.DBChan <- rec:
		default:
			log.Println("warning: DB write channel full, record dropped")
		}
	}()

	return nil
}

type channelReader struct {
	ch  <-chan []byte
	buf []byte
}

func newChannelReader(ch <-chan []byte) *channelReader {
	return &channelReader{ch: ch}
}

func (cr *channelReader) Read(p []byte) (int, error) {
	if len(cr.buf) > 0 {
		n := copy(p, cr.buf)
		cr.buf = cr.buf[n:]
		return n, nil
	}
	chunk, ok := <-cr.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		cr.buf = chunk[n:]
	}
	return n, nil
}

package proxy

import (
	"claude-proxy/parser"
	"claude-proxy/store"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type contextKey string

const requestStartKey contextKey = "requestStart"

type teeReadCloser struct {
	original         io.ReadCloser
	ch               chan []byte
	requestStartTime time.Time
	ttft             time.Duration
	ttftRecorded     bool
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
		if !r.ttftRecorded {
			r.ttft = time.Since(r.requestStartTime)
			r.ttftRecorded = true
		}
		cp := make([]byte, n)
		copy(cp, p[:n])
		select {
		case r.ch <- cp:
		default:
			log.Println("warning: parser channel full, data chunk dropped")
		}
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
	return r.ttft
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

func extractRateLimitHeaders(resp *http.Response) (retryAfter string, reqRemaining, tokRemaining int) {
	retryAfter = resp.Header.Get("Retry-After")
	reqRemaining = -1
	tokRemaining = -1
	if v := resp.Header.Get("X-Ratelimit-Requests-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			reqRemaining = n
		}
	}
	if v := resp.Header.Get("X-Ratelimit-Tokens-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tokRemaining = n
		}
	}
	return
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
	retryAfter, reqRemaining, tokRemaining := extractRateLimitHeaders(resp)

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
	inflightID := cfg.Store.AddInFlight(sessionID, endpoint)

	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(resp.Body, ch, requestStart)
	resp.Body = trc

	go func() {
		defer cfg.Store.RemoveInFlight(inflightID)

		var result parser.Result
		if isSSE {
			result, _ = parser.ParseSSE(newChannelReader(ch))
		} else {
			var buf []byte
			for chunk := range ch {
				buf = append(buf, chunk...)
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

# Claude Proxy — Token-Per-Minute Monitor

## Overview

A local HTTP reverse proxy (Go) that sits between Claude Code and the Anthropic API. It intercepts responses, extracts token usage, and displays live stats in a terminal dashboard (TUI). The goal is to measure token throughput per session and in aggregate.

## Architecture

```
Claude Code  ──HTTP──>  Proxy (localhost:8076)  ──HTTPS──>  api.anthropic.com
                              │
                        Parse response
                        Extract tokens
                              │
                         Stats Store
                        (in-memory, mutex)
                              │
                         Bubbletea TUI
                        (renders every 1s)
```

### Components

1. **Reverse Proxy** — `httputil.ReverseProxy` forwarding to `https://api.anthropic.com`. Uses `ModifyResponse` to set up response body interception (see Streaming Architecture below).
2. **Stats Store** — Mutex-protected struct holding per-session and aggregate stats. Sessions keyed by session/conversation ID from request headers.
3. **TUI** — `bubbletea` program running in the main goroutine, subscribing to store updates on a 1-second tick. Three panes: current session, request log, aggregate.
4. **Response Parser** — Extracts token fields and model name. Handles both regular JSON and streaming (SSE) responses.

### Streaming Architecture (Tee-Reader Pattern)

`resp.Body` is a single `io.ReadCloser` — it cannot be read twice. To parse tokens without disrupting the stream to Claude Code, `ModifyResponse` replaces `resp.Body` with a custom wrapper:

1. In `ModifyResponse`, wrap `resp.Body` with a tee-reader that duplicates all bytes to a side `io.Pipe`.
2. The proxy's normal copy loop reads the wrapper and forwards bytes to Claude Code as usual — no added latency.
3. A background goroutine reads from the pipe and parses SSE events (or JSON for non-streaming) to extract token usage.
4. When the stream ends (EOF), the goroutine sends the completed `RequestRecord` to the store.

```
resp.Body (original)
    │
    ├──> TeeReader wrapper ──> Claude Code (via proxy copy loop)
    │
    └──> io.Pipe ──> parser goroutine ──> Stats Store
```

For non-streaming responses, the same tee-reader pattern applies — the parser goroutine simply reads the full JSON body instead of scanning for SSE events.

### Startup Flow

1. Parse CLI flags (port, upstream URL)
2. Initialize stats store
3. Start HTTP server with reverse proxy handler in a goroutine
4. Run bubbletea TUI in the main goroutine
5. On TUI quit (`q`/`ctrl+c`), gracefully shut down the HTTP server. If there are in-flight requests, the TUI shows a "waiting for N in-flight request(s)..." message and waits up to 120 seconds for them to complete before force-closing. This prevents truncating long-running streaming responses (which can last 30-60s+ with extended thinking).

## Data Model

### RequestRecord

| Field          | Type          | Description                              |
|----------------|---------------|------------------------------------------|
| Timestamp      | time.Time     | When the request was sent                |
| Model          | string        | Model name from the response             |
| InputTokens    | int           | `usage.input_tokens`                     |
| OutputTokens   | int           | `usage.output_tokens` (includes thinking tokens) |
| CacheCreation  | int           | `usage.cache_creation_input_tokens`      |
| CacheRead      | int           | `usage.cache_read_input_tokens`          |
| Latency        | time.Duration | Request sent to response fully received  |
| Endpoint       | string        | e.g. `/v1/messages`                      |

Note: When extended thinking is enabled, thinking tokens are included in `output_tokens`. They are not broken out separately — this is intentionally out of scope for v1.

### Session

| Field     | Type              | Description                          |
|-----------|-------------------|--------------------------------------|
| ID        | string            | Session ID from request header       |
| StartTime | time.Time         | First request in this session        |
| LastSeen  | time.Time         | Most recent request in this session  |
| Requests  | []RequestRecord   | All completed requests               |

### Store

| Field    | Type                    | Description                   |
|----------|-------------------------|-------------------------------|
| mu       | sync.RWMutex            | Protects concurrent access    |
| sessions | map[string]*Session     | Keyed by session ID           |
| inflight | map[uint64]InFlightReq  | In-flight requests keyed by auto-increment ID |

### InFlightReq

| Field     | Type      | Description                    |
|-----------|-----------|--------------------------------|
| SessionID | string    | Which session this belongs to  |
| StartTime | time.Time | When the request was sent      |
| Endpoint  | string    | Request path                   |

## TPM Calculation

TPM measures throughput only when tokens are actively flowing. There is no inactivity threshold — active time is defined purely by request processing time.

**Active time** = sum of completed request latencies + sum of in-flight request durations (updated each tick)

**TPM** = `total_tokens / active_minutes`

Where `active_minutes = active_time_duration.Minutes()`

In-flight requests (sent but no response yet) contribute to active time in real-time. Once a response completes, that request's latency is locked in.

**Edge case:** If `active_minutes` is zero (no requests yet, or first tick), TPM is displayed as `--` in the TUI instead of computing a division by zero.

## Response Parsing

### Non-streaming responses

JSON body contains:
```json
{
  "usage": {
    "input_tokens": 1234,
    "output_tokens": 567,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 800
  },
  "model": "claude-sonnet-4-20250514"
}
```

The proxy reads the full body, extracts usage fields, and passes the body through unchanged.

### Streaming responses (SSE)

Token usage is spread across two SSE event types. The parser goroutine scans the tee'd stream for these events.

**`message_start`** — contains input tokens, cache tokens, and model:
```
event: message_start
data: {"type":"message_start","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":1234,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":800}}}
```

**`message_delta`** — final event, contains cumulative usage:
```
event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":567}}
```

Note: In `message_delta` events, `usage` is a top-level sibling of `delta`, not nested within it.

**Parser strategy — single source of truth:** The `message_delta` usage fields are cumulative. To avoid double-counting:
- Extract `model` from `message_start`.
- Extract `input_tokens`, `cache_creation_input_tokens`, and `cache_read_input_tokens` from `message_start` (these do not change during streaming).
- Extract `output_tokens` from the final `message_delta` only (cumulative, supersedes any earlier values).

The stream is forwarded to Claude Code in real-time via the tee-reader pattern — no buffering or added latency.

## TUI Layout

```
┌─ Current Session: abc-123 ─────────────── Duration: 12m 34s (active: 2m 08s) ┐
│ Input: 45,230    Output: 12,100    Cache Read: 8,400    Cache Create: 0       │
│ Total: 57,330 tok    TPM: 26,812    Requests: 14    Avg latency: 1.2s        │
├─ Recent Requests ─────────────────────────────────────────────────────────────┤
│ 12:03:45  /v1/messages  claude-sonnet-4-20250514  in:3200 out:890  cache:800  1.1s  │
│ 12:03:52  /v1/messages  claude-sonnet-4-20250514  in:3200 out:450  cache:0    0.9s  │
│ 12:04:01  /v1/messages  claude-sonnet-4-20250514  in:4100 out:1200 cache:800  1.4s  │
│ 12:04:15  /v1/messages  claude-sonnet-4-20250514  in:4100 out:--   ⏳ 3.2s...      │
├─ Aggregate (All Sessions) ────────────────────────────────────────────────────┤
│ Sessions: 3    Total: 234,500 tok    TPM: 28,100                              │
│ Requests: 87   Uptime: 1h 23m                                                │
└───────────────────────────────────────────────────────────────────────────────┘
```

### Interactions

- `q` / `ctrl+c` — quit (graceful shutdown)
- `tab` — cycle between sessions
- `j`/`k` — scroll the request log

### Refresh

TUI redraws on a 1-second tick.

## CLI Configuration

| Flag         | Default                         | Description            |
|--------------|---------------------------------|------------------------|
| `--port`     | `8076`                          | Proxy listen port      |
| `--upstream` | `https://api.anthropic.com`     | Upstream API URL       |
| `--session`  | `"default"`                     | Manual session name    |

### Usage

```bash
# Terminal 1: start the proxy
claude-proxy

# Terminal 2: point Claude Code at the proxy
ANTHROPIC_BASE_URL=http://localhost:8076 claude
```

## Session ID Extraction

The proxy checks request headers in this order for a session identifier:

1. `x-session-id`
2. `anthropic-session-id`

If none are found, requests are grouped under a `"default"` session.

**Note:** Claude Code may not currently send session ID headers. These header names are aspirational and will be verified empirically during development. As a practical fallback, a `--session` CLI flag allows the user to manually name the session when starting the proxy. Multiple proxy instances (each with a different `--session` name) can run on different ports to track separate Claude Code sessions.

## Project Structure

```
claude-proxy/
├── main.go              # CLI flags, wiring, startup/shutdown
├── proxy/
│   └── proxy.go         # ReverseProxy setup, ModifyResponse hook
├── parser/
│   ├── json.go          # Non-streaming response parser
│   └── sse.go           # SSE streaming response parser
├── store/
│   └── store.go         # Stats store, session management, TPM calculation
├── tui/
│   ├── tui.go           # Bubbletea model, update, view
│   └── styles.go        # Lipgloss styling
├── go.mod
└── go.sum
```

### Dependencies

- `github.com/charmbracelet/bubbletea` — TUI framework
- `github.com/charmbracelet/lipgloss` — TUI styling
- Standard library: `net/http`, `net/http/httputil`, `encoding/json`, `sync`

## No Persistence

All stats are ephemeral — they reset when the proxy restarts. No disk storage.

## No Cost Estimation

Cost estimation is explicitly out of scope.

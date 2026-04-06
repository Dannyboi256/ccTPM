# ccTPM

A local HTTP reverse proxy that sits between Claude Code and the Anthropic API, measuring per-direction throughput (ITPM, OTPM, RPM) with a live TUI dashboard, rate-limit header capture, and SQLite persistence for throttling detection.

## Why

Claude Code users have no visibility into whether Anthropic is throttling their usage. ccTPM intercepts every API response, extracts token counts and rate-limit headers, and computes real-time ITPM/OTPM/RPM using rolling 60-second windows — giving you a clear picture of throughput and throttling patterns across both API-key and OAuth authentication modes.

## Features

- **Per-direction throughput** — separate ITPM (input tokens/min), OTPM (output tokens/min), and RPM (requests/min) with rolling 60-second windows and peak tracking
- **Live TUI dashboard** — session stats with current/peak ITPM/OTPM/RPM, request log, and aggregate view updating every second
- **Rate-limit header capture** — extracts both API-key headers (`anthropic-ratelimit-{input,output}-tokens-*`) and OAuth unified headers (`anthropic-ratelimit-unified-{5h,7d}-*`)
- **Throttling detection** — tracks HTTP 429/529 status codes, TTFT (time-to-first-token), rate-limit headers, and SSE error events
- **Historical TPM analysis** — `--query tpm` with bucketed time-series and peak modes, groupable by session
- **SQLite persistence** — all request data and rate-limit headers stored for historical analysis
- **CLI query mode** — query sessions, requests, throttle events, summary stats, and TPM history from the command line
- **Zero latency overhead** — tee-reader pattern with buffered channels ensures the proxy never delays responses to Claude Code
- **Pure Go** — single binary, no CGo, no external dependencies at runtime

## Quick Start

```bash
# Build
go build -o ccTPM .

# Terminal 1: start the proxy
./ccTPM

# Terminal 2: point Claude Code at the proxy
ANTHROPIC_BASE_URL=http://localhost:8076 claude
```

## TUI Dashboard

```
+- Session: default [1/3] ----------- Duration: 12m 34s (active: 2m 08s) -------+
| In: 45.2K  Out: 12.1K  Cache-R: 8.4K  Cache-W: 0                            |
| Total: 57.3K tok  Reqs: 14  Avg latency: 1.2s                               |
|                                                                               |
|  ITPM  |  current: 26,812     peak: 34,500  (12:01:15)                       |
|  OTPM  |  current: 8,400      peak: 12,100  (12:01:15)                       |
|  RPM   |  current: 8          peak: 14      (12:02:30)                       |
+-------------------------------------------------------------------------------+
 Recent Requests
 12:03:45  /v1/messages  claude-sonnet-4-20250514  in:3200  out:890   cache:800  1.1s
 12:03:52  /v1/messages  claude-sonnet-4-20250514  in:3200  out:450   cache:0    0.9s
 12:04:15  /v1/messages  claude-sonnet-4-20250514  in:4100  out:--    in-flight  3.2s...
 12:04:30  /v1/messages  --                        429  retry:30s
 12:04:35  /v1/messages  claude-sonnet-4-20250514  ERR  overloaded   0.8s
+- Aggregate (All Sessions) ------------------------------------------------+
| Sessions: 3  Total: 234.5K tok  Requests: 87  Uptime: 1h23m0s             |
|                                                                            |
|  ITPM  |  current: 52,300     peak: 68,000  (11:45:10)                    |
|  OTPM  |  current: 18,200     peak: 24,500  (11:45:10)                    |
|  RPM   |  current: 12         peak: 22      (11:50:00)                    |
+----------------------------------------------------------------------------+
```

**Keys:**
- `q` / `ctrl+c` — quit (waits for in-flight requests)
- `tab` — cycle between sessions
- `j`/`k` or `up`/`down` — scroll request log

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8076` | Proxy listen port |
| `--upstream` | `https://api.anthropic.com` | Upstream API URL |
| `--session` | `default` | Session name |
| `--db` | `~/.claude-proxy/data.db` | SQLite database path |
| `--query` | | Query mode: `sessions`, `requests`, `throttle`, `summary`, `tpm` |
| `--from` | | Start time filter (e.g., `2026-03-30 09:00`) |
| `--to` | | End time filter |
| `--last` | | Relative time filter (e.g., `24h`, `7d`) |
| `--ttft-threshold` | `5000` | TTFT threshold in ms for `--query throttle` |
| `--limit` | per-mode | Max rows to return (default: 10 for most modes, unlimited for tpm buckets) |
| `--bucket` | `300` | Bucket size in seconds for `--query tpm` |
| `--peak` | `false` | Show peak values instead of time-series for `--query tpm` |
| `--group-by` | | Group results by dimension (e.g., `session`) for `--query tpm --peak` |

## Querying Historical Data

When `--query` is provided, the proxy does not start — it queries the database and exits.

```bash
# Session summary
./ccTPM --query sessions

# Requests in a time range
./ccTPM --query requests --from "2026-03-30 09:00" --to "2026-03-30 17:00"

# Throttling events (429s, 529s, SSE errors, high TTFT)
./ccTPM --query throttle --last 24h
./ccTPM --query throttle --ttft-threshold 3000

# Aggregate summary
./ccTPM --query summary --last 7d

# TPM time-series in 5-minute buckets (default)
./ccTPM --query tpm --last 24h

# TPM time-series in 1-hour buckets
./ccTPM --query tpm --bucket 3600 --last 7d

# Peak TPM across all sessions
./ccTPM --query tpm --peak --last 24h

# Peak TPM grouped by session
./ccTPM --query tpm --peak --group-by session --last 24h
```

You can also query the SQLite database directly:

```bash
sqlite3 ~/.claude-proxy/data.db "SELECT * FROM requests ORDER BY start_time DESC LIMIT 10"
```

## Throttling Detection

ccTPM captures multiple signals to help detect throttling:

| Signal | What it means |
|--------|--------------|
| **HTTP 429** | Explicit rate limit — `Retry-After` header captured |
| **HTTP 529** | Server overloaded |
| **SSE error event** | `overloaded_error` mid-stream (status 200 but no output) |
| **High TTFT** | API accepts request but delays token generation (soft throttling) |
| **API-key rate headers** | `anthropic-ratelimit-{input,output}-tokens-{limit,remaining,reset}`, `anthropic-ratelimit-requests-*` |
| **OAuth unified headers** | `anthropic-ratelimit-unified-{5h,7d}-{utilization,reset,status}`, `unified-status`, `unified-representative-claim` |
| **Legacy headers** | `x-ratelimit-requests-remaining`, `x-ratelimit-tokens-remaining` (fallback) |

## Throughput Metrics

ccTPM tracks three per-direction throughput metrics using a **rolling 60-second window**:

| Metric | Definition |
|--------|-----------|
| **ITPM** | Input tokens per minute — `Σ(input_tokens + cache_creation_tokens)`. Cache-read tokens are deliberately excluded per Anthropic's ITPM definition. |
| **OTPM** | Output tokens per minute — `Σ(output_tokens)` |
| **RPM** | Requests per minute — count of all completed requests (including errors) |

Metrics are attributed to the request's **end time** (when tokens are counted against your rate limit).

**Peak tracking**: the TUI records the highest ITPM, OTPM, and RPM observed per session and across all sessions, with timestamps.

When no requests have completed yet, metrics display `--`.

## Architecture

```
Claude Code  --HTTP-->  ccTPM (localhost:8076)  --HTTPS-->  api.anthropic.com
                              |
                        Tee-reader pattern
                        (buffered channel)
                              |
                    +---------+---------+
                    |                   |
              Parser goroutine    Claude Code
              (extracts tokens)   (gets response
                    |              unmodified)
                    |
              +-----+-----+
              |           |
         In-memory     SQLite
          Store         DB
           |
          TUI
```

## Multiple Sessions

Track separate Claude Code sessions by running multiple proxy instances:

```bash
# Terminal 1
./ccTPM --session project-a --port 8076

# Terminal 2
./ccTPM --session project-b --port 8077

# Query across all sessions
./ccTPM --query sessions
```

## Requirements

- Go 1.25+

## Dependencies

- [bubbletea v2](https://charm.land/bubbletea) — TUI framework
- [lipgloss v2](https://charm.land/lipgloss) — Terminal styling
- [modernc.org/sqlite](https://modernc.org/sqlite) — Pure Go SQLite

## License

MIT

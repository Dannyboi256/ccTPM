# ccTPM

A local HTTP reverse proxy that sits between Claude Code and the Anthropic API, tracking token usage per minute with a live TUI dashboard and SQLite persistence for throttling detection.

## Why

Claude Code users have no visibility into whether Anthropic is throttling their usage. ccTPM intercepts every API response, extracts token counts, and computes real-time TPM (tokens per minute) using merged time intervals — giving you a clear picture of throughput and throttling patterns.

## Features

- **Live TUI dashboard** — session stats, request log, and aggregate view updating every second
- **Accurate TPM** — uses merged time intervals to correctly handle concurrent requests
- **Throttling detection** — tracks HTTP 429/529 status codes, TTFT (time-to-first-token), rate-limit headers, and SSE error events
- **SQLite persistence** — all request data stored for historical analysis
- **CLI query mode** — query sessions, requests, throttle events, and summary stats from the command line
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
+- Session: default ----------------------- Duration: 12m 34s (active: 2m 08s) -+
| In: 45.2K  Out: 12.1K  Cache-R: 8.4K  Cache-W: 0                            |
| Total: 57.3K tok  TPM: 26,812  Reqs: 14  Avg latency: 1.2s                   |
+-------------------------------------------------------------------------------+
 Recent Requests
 12:03:45  /v1/messages  claude-sonnet-4-20250514  in:3200  out:890   cache:800  1.1s
 12:03:52  /v1/messages  claude-sonnet-4-20250514  in:3200  out:450   cache:0    0.9s
 12:04:15  /v1/messages  claude-sonnet-4-20250514  in:4100  out:--    in-flight  3.2s...
 12:04:30  /v1/messages  --                        429  retry:30s
 12:04:35  /v1/messages  claude-sonnet-4-20250514  ERR  overloaded   0.8s
+- Aggregate (All Sessions) ------------------------------------------------+
| Sessions: 3  Total: 234.5K tok  TPM: 28,100                               |
| Requests: 87  Uptime: 1h23m0s                                             |
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
| `--query` | | Query mode: `sessions`, `requests`, `throttle`, `summary` |
| `--from` | | Start time filter (e.g., `2026-03-30 09:00`) |
| `--to` | | End time filter |
| `--last` | | Relative time filter (e.g., `24h`, `7d`) |
| `--ttft-threshold` | `5000` | TTFT threshold in ms for `--query throttle` |

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
| **Rate-limit headers** | `x-ratelimit-requests-remaining`, `x-ratelimit-tokens-remaining` |

## TPM Calculation

TPM uses **merged time intervals** to accurately measure throughput regardless of request concurrency:

- Two sequential 30s requests = 60s active time
- Two concurrent 30s requests = 30s active time (not 60s)
- Only time when requests are actively processing counts — idle gaps are excluded

When no requests have completed yet, TPM displays `--`.

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

- Go 1.22+

## Dependencies

- [bubbletea v2](https://charm.land/bubbletea) — TUI framework
- [lipgloss v2](https://charm.land/lipgloss) — Terminal styling
- [modernc.org/sqlite](https://modernc.org/sqlite) — Pure Go SQLite

## License

MIT

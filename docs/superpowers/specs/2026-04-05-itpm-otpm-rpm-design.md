# ccTPM — ITPM / OTPM / RPM Throughput Measurement

## Goal

Turn ccTPM into a longitudinal measurement instrument for the **actual token throughput** Claude Code achieves under OAuth authentication. The user wants to observe normal-use throughput, compare it across days and sessions, and let the *maximum* TPM emerge empirically as usage scales — not set a budget target, not hit a synthetic benchmark, not alert on limits.

Today ccTPM reports a single "TPM" number computed as `total_tokens / active_time_minutes` (where active time is merged request intervals). That formula does not match Anthropic's TPM definition, mixes all four token categories, and answers no question the user actually has. This design replaces it with three rolling-60-second metrics that match Anthropic's documented rate-limit semantics: ITPM, OTPM, and RPM.

A secondary goal — bundled because the relevant code path is already being edited — is to fix the proxy's rate-limit header capture. The proxy currently reads `X-Ratelimit-Requests-Remaining` / `X-Ratelimit-Tokens-Remaining`, which are not the header names Anthropic emits. Anthropic returns `anthropic-ratelimit-input-tokens-*` / `anthropic-ratelimit-output-tokens-*` for API-key traffic and a separate `anthropic-ratelimit-unified-*` set for OAuth Claude Code traffic. The current DB columns are therefore always populated with the `-1` fallback; they convey no information. This design fixes the header names and adds columns for the unified OAuth headers.

## Non-goals

- **No budget/alert feature.** ccTPM does not warn the user when they are approaching a limit. It measures, it does not gate.
- **No synthetic load generation.** ccTPM observes traffic that naturally flows through the proxy.
- **No chart/sparkline rendering in query output.** Plain tabwriter tables, matching existing `--query` modes.
- **No JSON or CSV export.** Follow-up work if needed.
- **No TUI automated tests.** Consistent with the existing `tui/` package which has no tests. Manual verification only.
- **No removal of the old per-request rate-limit columns (`ratelimit_req_remaining`, `ratelimit_tok_remaining`).** They stay in place for backward compatibility with existing local DBs and get populated from the fixed headers as synonyms.

## Three metrics, formally defined

All three use a fixed rolling 60-second window ending at the query moment `now`. A request contributes to the window if its `end_time` falls inside `(now-60s, now]`.

```
ITPM(sessionID, now) = Σ (InputTokens + CacheCreation)   for records whose end_time ∈ (now-60s, now]
OTPM(sessionID, now) = Σ (OutputTokens)                  for records whose end_time ∈ (now-60s, now]
RPM(sessionID, now)  = count of records                  whose end_time ∈ (now-60s, now]
```

Because the window is exactly one minute wide, the token sum *is* the per-minute rate; no division by elapsed time is needed.

### Why end-time attribution

A single response's usage total is only known when the response completes. For SSE streaming that is the final `message_delta` event; for JSON responses it is when the body is fully read. Attributing tokens to the minute in which the end event occurred matches how Anthropic charges (usage is emitted at response completion) and avoids fractional attribution logic across minute boundaries.

**Known limitation:** a 45-second streaming request whose start was in one minute and end in the next is fully attributed to the ending minute. For the bucket granularities the user cares about (minutes/hours/days) this is negligible.

### Why ITPM includes cache_creation but not cache_read

Matches Anthropic's official ITPM definition for modern models (Claude Sonnet 4.x, Opus 4.x, Haiku 4.5). Per the docs, `input_tokens + cache_creation_input_tokens` count toward ITPM rate limits; `cache_read_input_tokens` does not. Keeping the same definition means the numbers ccTPM reports are comparable against Anthropic's published tier tables if the user ever wants to cross-reference. Cache-read volume is still captured and displayed in a secondary totals line for visibility.

### Why in-flight requests are excluded from rolling metrics

A request with no final token count cannot contribute to ITPM/OTPM. The existing in-flight display in the TUI continues to show count and elapsed time separately (a visibility feature), but in-flight requests do not affect rolling metric values.

## Architecture and package touchpoints

| Package | Change type | Summary |
|---|---|---|
| `store/` | Replace + add | Remove `CalculateTPM` / `CalculateAggregateTPM`. Add `RollingITPM`, `RollingOTPM`, `RollingRPM` and aggregate variants. Add `SessionPeaks` struct, per-session and per-Store. Add new nullable rate-limit-header fields to `RequestRecord`. |
| `proxy/` | Fix + extend | Fix `extractRateLimitHeaders` to read `anthropic-ratelimit-*` instead of `X-Ratelimit-*`. Capture unified-* headers used by OAuth Claude Code. |
| `db/` | Migrate + add | Additive `ALTER TABLE` migration for new header columns. New `QueryTPMBuckets` and `QueryTPMPeak` methods. |
| `query/` | New command | New `tpm` sub-mode with `--last`, `--from`, `--to`, `--bucket`, `--peak`, `--group-by`, `--session`, `--limit` flags. |
| `tui/` | Replace panel | Replace session pane's single TPM display with three-row ITPM/OTPM/RPM block showing current and session-peak. Same for aggregate pane. |
| `parser/` | No change | Already extracts all four token types correctly. |
| `main.go` | Wire-up only | Register `tpm` query mode, no architectural change. |

### No materialized TPM buckets

Considered and rejected: a background goroutine writing a `tpm_minutes` aggregate table. Every request already has `start_time`, `end_time`, and token counts, so rolling metrics at any historical moment are exactly recoverable from the raw `requests` table via SQL. The `idx_requests_start_time` index is sufficient for the bucket sizes the user will query. Avoiding the aggregate table eliminates a second source of truth and a write-path complication.

Trade-off: historical query mode computes rates on demand at query time. For the expected data volumes (weeks of Claude Code usage, low-thousands of records/day) this is a few milliseconds.

## Peak tracking

Peak is a display-layer concern computed at TUI-tick cadence (currently 1 second), not a storage concern.

```go
type SessionPeaks struct {
    MaxITPM     float64
    MaxITPMTime time.Time
    MaxOTPM     float64
    MaxOTPMTime time.Time
    MaxRPM      int
    MaxRPMTime  time.Time
}
```

`SessionPeaks` is attached to the `Session` struct and protected by the existing `store.mu`. On each TUI tick:

1. Compute `Rolling{ITPM,OTPM,RPM}` for the visible session and for the aggregate.
2. Under `store.mu.Lock`, compare each value against the stored peak; update peak value and timestamp if exceeded.
3. Render.

Peaks reset when the session is pruned at the 1-hour inactivity boundary (existing lifecycle, unchanged). Aggregate peaks reset when the proxy restarts. This is intentional — peaks are session-scoped, not all-time. All-time peaks are recovered from the DB via `--query tpm --peak`.

**Why not compute peaks in the DB writer path instead?** Because peak is defined over a window, not over a single record. You cannot know ITPM from a single record alone. The tick path already has the correct cadence and the aggregation logic lives in one place.

## Historical query and SQL approach

`QueryTPMBuckets(from, to time.Time, bucketSeconds int) ([]TPMBucket, error)` computes per-bucket max rates via a two-stage SQL query.

**Why a new `end_time_epoch` integer column is required.** The obvious approach — `strftime('%s', end_time)` — **does not work** with the `modernc.org/sqlite` driver. When `db.InsertRecord` binds a Go `time.Time` through a `?` placeholder, the driver stores it using Go's default `time.Time.String()` format: `"2026-04-05 14:23:45 +0000 UTC"`. SQLite's `strftime` cannot parse the trailing ` +0000 UTC` suffix and returns NULL. The driver has a special DATETIME→string read-side conversion that rewrites the stored bytes into RFC3339 when you `SELECT end_time` into a Go string, but that conversion is not applied when SQLite's date functions look at the raw column bytes — so the data looks fine via `scanRecords` but is invisible to `strftime`.

To sidestep this entirely, a dedicated `end_time_epoch INTEGER` column is added to the schema. It is populated at insert time from `rec.EndTime.UTC().Unix()` — an unambiguous integer, trivially indexable, no format ambiguity. The SQL below uses that column directly; `strftime` is not called anywhere in the TPM query path.

Rows predating the migration will have `end_time_epoch = NULL` and will be silently excluded from TPM queries. This is acceptable: pre-migration rows also predate the header-capture fix, and their TPM values computed from the old broken `CalculateTPM` formula are not comparable to the new metric anyway. Documented as a known limitation.

```sql
WITH minute_windows AS (
  SELECT
    (end_time_epoch / 60) * 60                      AS minute_epoch,
    SUM(input_tokens + cache_creation)              AS itpm,
    SUM(output_tokens)                              AS otpm,
    COUNT(*)                                        AS rpm
  FROM requests
  WHERE end_time_epoch BETWEEN ? AND ?
    AND end_time_epoch IS NOT NULL
  GROUP BY minute_epoch
)
SELECT
  (minute_epoch / ?) * ? AS bucket_epoch,
  MAX(itpm)              AS max_itpm,
  MAX(otpm)              AS max_otpm,
  MAX(rpm)               AS max_rpm,
  COUNT(*)               AS sample_minutes
FROM minute_windows
GROUP BY bucket_epoch
ORDER BY bucket_epoch;
```

The inner CTE produces per-minute rates keyed on minute-of-end (integer seconds since epoch, floored to minute). The outer query buckets those minutes at the requested granularity and selects the peak. `sample_minutes` is exposed so the user can spot buckets with thin data (e.g., a 1h bucket with only 3 active minutes).

**Caveat on approximation:** this groups by "minute the request ended in" rather than a true sliding 60-second window. A burst of 30 requests in seconds 55–65 of a minute would split across two minute rows (say, 10 in minute N and 20 in minute N+1) rather than showing as one 30-request rolling window. For bucket sizes of 5 minutes or larger — which covers every realistic user query — the error is negligible. A true sliding window would require a per-second base table and window functions, which is overkill for this use case.

`QueryTPMPeak(from, to time.Time, groupBy string) ([]TPMPeak, error)` either returns a single-row all-time peak (one row per metric, joined with the record's session and timestamp) or, with `groupBy = "session"`, one row per session_id with each session's peak values.

## Schema migration

Additive, no destructive changes, executed inline during `createSchema()` at startup.

```sql
-- Executed after CREATE TABLE IF NOT EXISTS; each statement is wrapped in
-- "ignore 'duplicate column' error" so repeated runs are idempotent.
ALTER TABLE requests ADD COLUMN end_time_epoch      INTEGER;   -- unix seconds, NULL for pre-migration rows
CREATE INDEX IF NOT EXISTS idx_requests_end_time_epoch ON requests(end_time_epoch);
ALTER TABLE requests ADD COLUMN itokens_limit       INTEGER;
ALTER TABLE requests ADD COLUMN itokens_remaining   INTEGER;
ALTER TABLE requests ADD COLUMN itokens_reset       TEXT;      -- RFC3339
ALTER TABLE requests ADD COLUMN otokens_limit       INTEGER;
ALTER TABLE requests ADD COLUMN otokens_remaining   INTEGER;
ALTER TABLE requests ADD COLUMN otokens_reset       TEXT;
ALTER TABLE requests ADD COLUMN rpm_limit           INTEGER;
ALTER TABLE requests ADD COLUMN rpm_remaining       INTEGER;
ALTER TABLE requests ADD COLUMN rpm_reset           TEXT;
ALTER TABLE requests ADD COLUMN unified_5h_util     REAL;
ALTER TABLE requests ADD COLUMN unified_5h_reset    INTEGER;   -- unix seconds
ALTER TABLE requests ADD COLUMN unified_5h_status   TEXT;
ALTER TABLE requests ADD COLUMN unified_7d_util     REAL;
ALTER TABLE requests ADD COLUMN unified_7d_reset    INTEGER;
ALTER TABLE requests ADD COLUMN unified_7d_status   TEXT;
ALTER TABLE requests ADD COLUMN unified_status      TEXT;
ALTER TABLE requests ADD COLUMN unified_repr_claim  TEXT;
```

All new columns are nullable. Rationale for nullable over sentinel: `unified_5h_util` is a float fraction where `0.0` is a legal value ("0% used"), so we cannot distinguish "zero utilization" from "header absent" with a `-1`/`0` sentinel. For consistency, the integer token-limit columns also use NULL rather than `-1`.

The existing `ratelimit_req_remaining` and `ratelimit_tok_remaining` columns are preserved and populated from their natural legacy headers:

- `ratelimit_req_remaining` ← `anthropic-ratelimit-requests-remaining` (same value as new `rpm_remaining`).
- `ratelimit_tok_remaining` ← `anthropic-ratelimit-tokens-remaining` — the legacy combined "tokens remaining" header that Anthropic still emits (per the docs: "the most restrictive limit currently in effect, or the sum of input and output tokens if Workspace limits do not apply"). This is *not* the same as `itokens_remaining` — it is a separate header Anthropic continues to publish for backward compatibility. ccTPM stores it as-is in the legacy column.

This keeps any existing user scripts against those columns working with no behavior change, and also preserves the legacy combined "tokens remaining" signal alongside the new per-direction fields.

Idempotence: SQLite returns `SQLITE_ERROR` with message `duplicate column name: X` if a column already exists. The migration runner checks for that specific error text and continues past it. A schema_version table is not introduced — this is the only migration and a text-match check is simpler than bringing in a migration library.

## Header parsing

Parsed in `proxy/extractRateLimitHeaders`, which now returns a richer struct rather than three scalars.

```go
type RateLimitHeaders struct {
    // Per-direction API-key headers
    ITokensLimit      *int
    ITokensRemaining  *int
    ITokensReset      string     // RFC3339 as emitted by API
    OTokensLimit      *int
    OTokensRemaining  *int
    OTokensReset      string
    RPMLimit          *int
    RPMRemaining      *int
    RPMReset          string

    // Unified OAuth headers (Pro/Max subscription)
    Unified5hUtil     *float64
    Unified5hReset    *int64     // unix seconds
    Unified5hStatus   string
    Unified7dUtil     *float64
    Unified7dReset    *int64
    Unified7dStatus   string
    UnifiedStatus     string     // "allowed" | "rate_limited"
    UnifiedReprClaim  string     // e.g. "five_hour"

    // Legacy (unchanged)
    RetryAfter        string
}
```

Pointer types for numeric fields so "absent" and "zero" are distinguishable in both the DB round-trip and the TUI rendering code.

Parse rules:

| Header | Type | Rule | On malformed/absent |
|---|---|---|---|
| `anthropic-ratelimit-input-tokens-limit` | int | `strconv.Atoi` | `nil` |
| `anthropic-ratelimit-input-tokens-remaining` | int | `strconv.Atoi` | `nil` |
| `anthropic-ratelimit-input-tokens-reset` | string | store raw | `""` |
| `anthropic-ratelimit-output-tokens-*` | same as above | | |
| `anthropic-ratelimit-requests-*` | same as above | | |
| `anthropic-ratelimit-unified-5h-utilization` | float | `strconv.ParseFloat(s, 64)` | `nil` |
| `anthropic-ratelimit-unified-5h-reset` | int64 | try `ParseInt` (unix seconds), fall back to `time.Parse(time.RFC3339)` then `.Unix()` | `nil` |
| `anthropic-ratelimit-unified-5h-status` | string | store raw | `""` |
| `anthropic-ratelimit-unified-7d-*` | same as 5h | | |
| `anthropic-ratelimit-unified-status` | string | store raw | `""` |
| `anthropic-ratelimit-unified-representative-claim` | string | store raw | `""` |
| `Retry-After` | string | unchanged | `""` |

Malformed headers never surface as user-visible errors. They log at debug level (if a logger is available) and fall through to NULL/empty.

## Data flow (one completed request)

```
Claude Code
   │
   ▼
Proxy.Rewrite  ── records start_time on request context
   │
   ▼
Anthropic API
   │
   ▼
Proxy.ModifyResponse
   │
   ├── extractRateLimitHeaders(resp)   [FIXED: anthropic-ratelimit-* + unified-*]
   │      → populates all new header fields on RequestRecord
   │
   ├── tee response body ─── parser goroutine
   │                             │
   │                             ├── parser.ParseSSE or parser.ParseJSON
   │                             │     → InputTokens, OutputTokens,
   │                             │       CacheCreation, CacheRead, Model
   │                             │
   │                             └── store.AddRecord(rec)   [in-memory]
   │                                 dbChan <- rec          [buffered DB write]
   │
   └── return (response flows to Claude Code unmodified)

TUI tick (1 second):
   for currentSession:
     itpm = store.RollingITPM(sessionID, now)
     otpm = store.RollingOTPM(sessionID, now)
     rpm  = store.RollingRPM(sessionID, now)
     updateSessionPeaks(sessionID, itpm, otpm, rpm, now)
   for aggregate:
     same three calls on *Aggregate variants
     updateAggregatePeaks(...)
   render
```

## Concurrency

- `store.mu` remains the single mutex guarding `sessions`, `inflight`, and now also `SessionPeaks` per session and the new `Store.AggregatePeaks`.
- New rolling methods take `RLock` once, iterate the session's (or all sessions') `Requests` slice, release. Bounded by `≤1000` records per session (existing cap) so iteration is cheap.
- Peak updates happen from the TUI goroutine only (single writer). They take `Lock` for the compare-and-swap window. No other writer touches peaks.
- `AddRecord` continues to take `Lock`; rolling reads take `RLock`; neither blocks the other's progress beyond the brief critical section.
- `integration_test.go` gets a concurrent-reader test run under `-race`.

## Edge cases

1. **Zero-token error responses** (e.g., a 400 that still returned a `usage` block with zeros, or a request that errored before generating output) — contribute to RPM, contribute 0 to ITPM/OTPM. Not peak triggers.
2. **Retried request after 429** — each attempt creates its own record and each counts independently toward RPM. That is the honest measurement; Anthropic also counts retries separately.
3. **Clock skew / out-of-order `end_time`** — rolling window uses `time.Now()` at call time; records with `end_time > now` are excluded by the `!end.After(now)` check. No corruption.
4. **Clock jump backward** (NTP correction, laptop sleep) — can briefly cause records with `end_time > now` to exist. They are excluded until `now` catches up, which happens within 60s naturally.
5. **Empty session** (TUI render before first completed request) — all metrics display `--`, peaks display `--`. Distinct from "0" which would imply zero throughput.
6. **Very long SSE stream (>60s)** — tokens land in the minute of stream-end, not smeared. Creates a visible spike at the end minute. Matches how Anthropic reports usage. Documented known limitation.
7. **In-flight at shutdown** — existing graceful-shutdown path (wait for in-flight, drain dbChan, 120s timeout) is unchanged. New metrics do not block shutdown.
8. **Session pruning mid-render** — existing race-free pattern via `GetSession` (which copies). New peak data is part of the copied session, so a TUI render using a freshly-pruned session reads stale-but-consistent data and the next tick heals it.
9. **Malformed rate-limit header values** — field is NULL/empty, record still stores fine, no user-visible error.
10. **Mixed header sets on same response** (hypothetical — OAuth response that also emits API-key headers) — all fields get populated; no conflict. The TUI only reads the fields it displays; other fields sit in the DB for future analysis.

## User-facing changes

### TUI — session pane

Before (`tui.go:164–175`):
```
 In: 145k  Out: 23k  Cache-R: 2.4M  Cache-W: 180k
 Total: 2.75M tok  TPM: 219500  Reqs: 234  Avg latency: 4.2s
```

After:
```
 Session: my-session                          Duration: 12m 34s (active: 8m 12s)
 ─────────────────────────────────────────────────────────────────────────
  ITPM  │  current:   12.4k     peak:   45.2k  (14:23:11)
  OTPM  │  current:    2.1k     peak:    8.7k  (14:22:58)
  RPM   │  current:      18     peak:      42  (14:23:11)
 ─────────────────────────────────────────────────────────────────────────
  Totals: In 145k  Out 23k  Cache-R 2.4M  Cache-W 180k  Reqs 234  Avg 4.2s
```

- `current` = rolling-60s value at the render moment.
- `peak` = session-wide maximum observed so far, with local-time `HH:MM:SS` timestamp.
- `--` when no data (current and peak both `--` for a session with zero completed requests).
- Numbers use the existing `formatNum` helper (K/M suffixes ≥1000/1000000).
- Layout width ~72 columns, fits in 80-col terminals.

### TUI — aggregate pane

Same three-row structure, but computed across all sessions. Aggregate peaks are stored on the `Store` and reset on proxy restart.

```
 Aggregate (All Sessions)                     Uptime: 1h 23m
 ─────────────────────────────────────────────────────────────────────────
  ITPM  │  current:   34.1k     peak:  127.8k  (13:55:02)
  OTPM  │  current:    5.4k     peak:   18.3k  (13:55:02)
  RPM   │  current:      48     peak:     101  (14:10:44)
 ─────────────────────────────────────────────────────────────────────────
  Sessions: 5   Requests: 1847   Total tok: 18.2M
```

### Query command — `--query tpm`

Three sub-modes via flags.

**1. Bucketed historical table (default):**
```
$ ./ccTPM --query tpm --last 24h --bucket 1h
BUCKET_START         MAX_ITPM  MAX_OTPM  MAX_RPM  SAMPLES
2026-04-04 15:00     12400     2100      18       47
2026-04-04 16:00     45200     8700      42       312
2026-04-04 17:00     8100      1800      14       89
...
2026-04-05 14:00     31500     6200      28       245
```

**2. All-time peak:**
```
$ ./ccTPM --query tpm --peak
METRIC  VALUE   OBSERVED AT          SESSION
ITPM    45200   2026-04-04 14:23     my-session
OTPM    8700    2026-04-04 14:22     my-session
RPM     42      2026-04-04 14:23     my-session
```

**3. Per-session peak:**
```
$ ./ccTPM --query tpm --peak --group-by session --last 7d
SESSION       MAX_ITPM  MAX_OTPM  MAX_RPM  FIRST_SEEN        LAST_SEEN
my-session    45200     8700      42       2026-04-04 10:00  2026-04-04 18:30
other-sess    31500     6200      28       2026-04-03 09:15  2026-04-03 17:45
```

**Flag set:**

| Flag | Default | Purpose |
|---|---|---|
| `--last` | `1h` | Time range ending now (`1h`, `24h`, `7d`) |
| `--from` / `--to` | unset | Explicit range, overrides `--last` |
| `--bucket` | `5m` | Bucket size for bucketed table mode |
| `--peak` | `false` | Switch to single-row peak output |
| `--group-by` | unset | With `--peak`: `session` groups by session_id |
| `--session` | unset | Filter to one session |
| `--limit` | `0` (unlimited) for bucketed mode; `10` for `--peak` modes | Row cap. Bucketed time-series defaults to unlimited because 10 rows is too few for any useful comparison (e.g. `--last 24h --bucket 1h` needs 24 rows). Peak modes keep the project's `10` convention. User can always override with `--limit N`. |

Output uses `text/tabwriter` to match the style of existing `--query sessions` / `requests` / `throttle`.

**Timezone of output.** SQL operates in UTC (all `start_time` / `end_time` values are stored as UTC by `db.InsertRecord`). For user-facing output, `BUCKET_START` and `OBSERVED AT` columns are rendered in the machine's **local timezone**, matching how `--query requests` and `--query throttle` already format timestamps (`r.StartTime.Format("15:04:05")` uses the `time.Time`'s local zone). Internal bucket boundaries are still computed in UTC seconds so bucket membership is stable across DST transitions; only the display is localized.

## Testing strategy

Split into unit tests per package and integration tests at the top level, matching the existing suite structure.

### `store/store_test.go` — new tests

- `TestRollingITPM_WindowBoundary` — record ending at `now-59s` counts; `now-61s` does not.
- `TestRollingITPM_IncludesCacheCreation` — ITPM = InputTokens + CacheCreation, excludes CacheRead.
- `TestRollingOTPM_OutputOnly` — OTPM is exactly OutputTokens.
- `TestRollingRPM_CountsRequests` — RPM counts records regardless of token count (zero-token errors count).
- `TestRollingMetrics_EmptySession` — returns 0 for sessions with no records or all records outside window.
- `TestRollingMetrics_InFlightExcluded` — in-flight entries do not contribute.
- `TestSessionPeaks_UpdateOnIncrease` — peak values and timestamps update only when current exceeds stored peak.
- `TestSessionPeaks_PersistAcrossQuiet` — after throughput drops, peaks stay at historical max.
- `TestAggregatePeaks_MultipleSessions` — aggregate peaks reflect highest observed across any session.
- `TestRollingMetrics_ConcurrentReaders` — multiple readers + writer, `-race` clean.

### `parser/parser_test.go` — no new tests

Parser already correctly extracts all four token types. Existing tests cover JSON and SSE.

### `proxy/proxy_test.go` — new tests

- `TestExtractRateLimitHeaders_AnthropicNaming` — reads new header names correctly.
- `TestExtractRateLimitHeaders_UnifiedOAuth` — parses unified-* headers (floats, unix timestamps, strings).
- `TestExtractRateLimitHeaders_BothSets` — both header sets on one response populate both field groups.
- `TestExtractRateLimitHeaders_Missing` — absent headers → nil/empty, no panic.
- `TestExtractRateLimitHeaders_MalformedUtilization` — bad float → nil, no error surfaced.

### `db/db_test.go` — new tests

- `TestSchemaMigration_AddsNewColumns` — fresh DB gets all new columns.
- `TestSchemaMigration_Idempotent` — running `createSchema()` twice does not error.
- `TestSchemaMigration_PreservesExistingData` — old-schema DB with rows migrates cleanly.
- `TestInsertRecord_WithNullableHeaders` — record with some nil fields round-trips.
- `TestQueryTPMBuckets_BasicShape` — records across 3 minutes, 1-minute bucket, correct max values per bucket.
- `TestQueryTPMBuckets_EmptyBuckets` — range with no records → empty result.
- `TestQueryTPMBuckets_BucketBoundaryGrouping` — 14:59:59 groups into 14:00 under 1h bucket.
- `TestQueryTPMPeak_AllTime` — single-row result with highest observation across the range.
- `TestQueryTPMPeak_GroupBySession` — one row per session_id with session peak.

### `query/query_test.go` — new tests

- `TestRunTPM_BucketedTable` — expected header row + data row alignment.
- `TestRunTPM_Peak` — single-row output format.
- `TestRunTPM_PeakGroupBySession` — per-session row format.
- `TestRunTPM_FlagValidation` — mutually exclusive flags error clearly.
- `TestRunTPM_UnknownGroupBy` — rejects unknown `--group-by` values.

### `tui/` — no new automated tests

Consistent with current `tui/` package which has none. Manual verification via live Claude Code session.

### `integration_test.go` — new tests

- `TestProxyCapturesAnthropicRateLimitHeaders` — mock upstream with API-key headers → DB has populated columns.
- `TestProxyCapturesUnifiedHeaders` — mock upstream with unified-* headers → DB has populated columns.
- `TestEndToEndTPMMeasurement` — 10 mock requests over 30 seconds; verify `Rolling*` matches `QueryTPMBuckets` for the same window.

### Test helpers

Add `store.NewTestRecord(opts)` (in a `_test.go` file in package `store`) that builds a `RequestRecord` with sensible defaults and overrides. Reduces boilerplate across the new store and integration tests.

## Manual test checklist

1. `go build -o ccTPM .` succeeds.
2. `go test -v ./...` passes.
3. `go test -race ./...` passes.
4. Start proxy: `./ccTPM --port 8076 --session manual-test`.
5. Point Claude Code at `http://localhost:8076` and run a normal task.
6. Verify TUI shows ITPM, OTPM, RPM rows with live-updating `current` values.
7. Verify peak values persist and timestamps are sensible.
8. Aggregate pane updates alongside session pane.
9. `./ccTPM --query tpm --last 1h` returns a non-empty table.
10. `./ccTPM --query tpm --peak` returns three rows with the correct session id.
11. `sqlite3 ccTPM.db "SELECT unified_5h_util, unified_5h_status FROM requests ORDER BY start_time DESC LIMIT 5;"` shows non-NULL values for a real OAuth session (confirms header capture works end-to-end).

## Success criteria

- All automated tests pass, including `-race`.
- Manual TUI shows three metrics live-updating during a real Claude Code OAuth session.
- `--query tpm --peak` reports the user's observed maximum across recorded history.
- DB contains populated unified-* columns after an OAuth session, proving the header-capture fix works against real traffic.
- The old single "TPM" number is no longer displayed anywhere; the old `CalculateTPM` method is removed from `store/`.

## Open questions / deferred

- **JSON/CSV export from `--query tpm`** — deferred until the user needs to pipe data elsewhere.
- **Sparkline in TUI** — deferred; `current + peak` covers the immediate need.
- **Alert/notification when approaching limit** — explicitly out of scope; ccTPM is observe-only.
- **True sliding-window SQL for historical queries** — deferred; the GROUP-BY-minute approximation is adequate for the bucket sizes the user cares about.
- **A `tpm_minutes` materialized aggregate table** — not needed at current data volumes; revisit if query latency exceeds ~50ms.

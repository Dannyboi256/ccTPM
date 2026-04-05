# ITPM / OTPM / RPM Throughput Measurement — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace ccTPM's broken single-number "TPM" with three rolling-60-second metrics (ITPM, OTPM, RPM) matching Anthropic's documented definitions, plus fix the proxy's rate-limit header capture so OAuth Claude Code's unified-* headers and API-key per-direction headers are both stored.

**Architecture:** Rolling metrics computed on demand from the existing in-memory `Session.Requests` slice (no materialized aggregates). Peaks tracked at TUI tick cadence. Historical comparison via new `--query tpm` command computed from SQLite at query time. Additive schema migration with 17 new nullable columns; existing legacy columns preserved.

**Tech Stack:** Go 1.25, `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `modernc.org/sqlite`, `text/tabwriter`, stdlib `time`/`strconv`/`net/http`.

**Spec:** `docs/superpowers/specs/2026-04-05-itpm-otpm-rpm-design.md`

---

## File Structure

| File | Responsibility | Nature of change |
|---|---|---|
| `store/store.go` | In-memory storage + rolling metrics | Add 17 nullable header fields to `RequestRecord`. Add `SessionPeaks` struct, attach to `Session`. Add `Store.aggregatePeaks` field. Remove `CalculateTPM`/`CalculateAggregateTPM` (keep `GetActiveTime`, `mergeIntervals`, `sumDurations`, `collectIntervals` — still used by TUI duration display). Add `RollingITPM`/`RollingOTPM`/`RollingRPM` + `Aggregate*` variants. Add `UpdateSessionPeaks`/`UpdateAggregatePeaks`/`GetSessionPeaks`/`GetAggregatePeaks`. |
| `store/store_test.go` | Store tests | Delete the four old TPM tests (`TestCalculateTPM`, `TestCalculateTPMConcurrent`, `TestCalculateTPMZeroActiveTime`, `TestCalculateAggregateTPM`). Add new rolling/peaks tests. Add `newTestRecord` helper (unexported, package-local). |
| `proxy/proxy.go` | HTTP proxy + header extraction | New `RateLimitHeaders` struct. Rewrite `extractRateLimitHeaders` returning this struct. Update `modifyResponse` (two call sites: success and error paths) to plumb all fields onto `RequestRecord`. |
| `proxy/proxy_test.go` | Proxy tests | Add tests for the new header extraction behavior. |
| `db/db.go` | SQLite persistence | Additive `ALTER TABLE` migration in `createSchema`, idempotent via duplicate-column error tolerance. Update `InsertRecord` and `scanRecords` to handle new nullable columns (use `sql.NullInt64`/`sql.NullFloat64`/`sql.NullString`). New `QueryTPMBuckets` + `QueryTPMPeak` methods. New `TPMBucket` + `TPMPeak` result types. |
| `db/db_test.go` | DB tests | Migration tests, nullable round-trip tests, TPM query tests. |
| `query/query.go` | CLI query | New `tpm` case in `RunQuery`. New `runTPM` function dispatching three sub-modes (bucketed, peak, peak+group-by). Extend `Opts` struct with `Bucket`, `Peak`, `GroupBy` fields. |
| `query/query_test.go` | Query tests | Add TPM output format tests. |
| `tui/tui.go` | Terminal UI | Rewrite `renderSessionPane` body: three-row ITPM/OTPM/RPM block with current+peak, plus secondary totals line. Rewrite `renderAggregatePane` body similarly. Add peak-update call inside the tick handler in `Update`. |
| `main.go` | CLI entry | Add `--bucket`, `--peak`, `--group-by` flags. Wire into `query.Opts`. The `tpm` query mode uses existing `--from`/`--to`/`--last`/`--session`/`--limit` flags too. |
| `integration_test.go` | End-to-end tests | Three new integration tests for header capture and end-to-end TPM measurement. |

No new files are created. `NewTestRecord` helper lives inside `store/store_test.go` (unexported, package-local). Integration tests construct records inline when needed.

---

## Task 1: Add nullable rate-limit-header fields to `RequestRecord`

**Files:**
- Modify: `store/store.go:10-26`

Extends `RequestRecord` to hold the new header data. Uses pointer types for nullable numerics so "absent" is distinguishable from "zero". Existing fields (`RateLimitReqRemaining`, `RateLimitTokRemaining`) stay as-is for backward compat.

- [ ] **Step 1: Write a failing test that sets and reads the new fields**

Append to `store/store_test.go`:

```go
func TestRequestRecordNullableHeaderFields(t *testing.T) {
	iLimit := 450000
	iRemaining := 448500
	fiveHUtil := 0.0184
	fiveHReset := int64(1712345678)

	rec := RequestRecord{
		SessionID:         "s1",
		ITokensLimit:      &iLimit,
		ITokensRemaining:  &iRemaining,
		ITokensReset:      "2026-04-05T14:30:00Z",
		Unified5hUtil:     &fiveHUtil,
		Unified5hReset:    &fiveHReset,
		UnifiedStatus:     "allowed",
		UnifiedReprClaim:  "five_hour",
	}
	if rec.ITokensLimit == nil || *rec.ITokensLimit != 450000 {
		t.Fatalf("ITokensLimit not set correctly")
	}
	if rec.Unified5hUtil == nil || *rec.Unified5hUtil != 0.0184 {
		t.Fatalf("Unified5hUtil not set correctly")
	}
	if rec.UnifiedStatus != "allowed" {
		t.Fatalf("UnifiedStatus not set correctly")
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test -run TestRequestRecordNullableHeaderFields ./store`
Expected: compile error — `ITokensLimit`, `Unified5hUtil`, etc. are undefined fields.

- [ ] **Step 3: Add the fields to `RequestRecord`**

Replace the `RequestRecord` struct definition in `store/store.go` (starting at line 10) with:

```go
type RequestRecord struct {
	StartTime             time.Time
	EndTime               time.Time
	Model                 string
	InputTokens           int
	OutputTokens          int
	CacheCreation         int
	CacheRead             int
	Endpoint              string
	StatusCode            int
	TTFT                  time.Duration
	RetryAfter            string
	RateLimitReqRemaining int
	RateLimitTokRemaining int
	HasError              bool
	SessionID             string

	// Per-direction API-key rate-limit headers (anthropic-ratelimit-{input,output}-tokens-*)
	ITokensLimit     *int
	ITokensRemaining *int
	ITokensReset     string // RFC3339 as emitted by API
	OTokensLimit     *int
	OTokensRemaining *int
	OTokensReset     string
	RPMLimit         *int
	RPMRemaining     *int
	RPMReset         string

	// Unified OAuth rate-limit headers (anthropic-ratelimit-unified-*)
	Unified5hUtil    *float64
	Unified5hReset   *int64 // unix seconds
	Unified5hStatus  string
	Unified7dUtil    *float64
	Unified7dReset   *int64
	Unified7dStatus  string
	UnifiedStatus    string // "allowed" | "rate_limited"
	UnifiedReprClaim string // e.g., "five_hour"
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -run TestRequestRecordNullableHeaderFields ./store`
Expected: PASS.

- [ ] **Step 5: Run the full store test suite to catch regressions**

Run: `go test ./store`
Expected: PASS (all existing tests still work — new fields are zero-value in existing test records).

- [ ] **Step 6: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add nullable rate-limit-header fields to RequestRecord"
```

---

## Task 2: Add `newTestRecord` helper for store tests

**Files:**
- Modify: `store/store_test.go`

Reduces boilerplate in rolling/peak tests by providing a builder with sensible defaults.

- [ ] **Step 1: Write a failing test that uses the helper**

Append to `store/store_test.go`:

```go
func TestNewTestRecordDefaults(t *testing.T) {
	rec := newTestRecord()
	if rec.SessionID != "test-session" {
		t.Fatalf("expected default session 'test-session', got %q", rec.SessionID)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected default status 200, got %d", rec.StatusCode)
	}
	if rec.StartTime.IsZero() || rec.EndTime.IsZero() {
		t.Fatal("expected default timestamps to be set")
	}
	if !rec.EndTime.After(rec.StartTime) {
		t.Fatal("expected EndTime to be after StartTime")
	}
}

func TestNewTestRecordOverrides(t *testing.T) {
	at := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	rec := newTestRecord(
		withSession("alt"),
		withEndTime(at),
		withTokens(1000, 200, 50, 300),
	)
	if rec.SessionID != "alt" {
		t.Fatalf("expected 'alt', got %q", rec.SessionID)
	}
	if !rec.EndTime.Equal(at) {
		t.Fatalf("expected EndTime %v, got %v", at, rec.EndTime)
	}
	if rec.InputTokens != 1000 || rec.OutputTokens != 200 || rec.CacheCreation != 50 || rec.CacheRead != 300 {
		t.Fatalf("token overrides not applied: %+v", rec)
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test -run TestNewTestRecord ./store`
Expected: compile error — `newTestRecord`, `withSession`, `withEndTime`, `withTokens` undefined.

- [ ] **Step 3: Add the helper to `store/store_test.go`**

Append to the top of `store/store_test.go` (after the imports) — or in a dedicated helper block at the bottom of the file. Put it near the top for discoverability:

```go
// Test helpers — unexported, package-local.
type testRecordOpt func(*RequestRecord)

func newTestRecord(opts ...testRecordOpt) RequestRecord {
	base := time.Now()
	rec := RequestRecord{
		SessionID:  "test-session",
		StartTime:  base.Add(-1 * time.Second),
		EndTime:    base,
		Model:      "claude-sonnet-4-20250514",
		StatusCode: 200,
		Endpoint:   "/v1/messages",
	}
	for _, opt := range opts {
		opt(&rec)
	}
	return rec
}

func withSession(id string) testRecordOpt {
	return func(r *RequestRecord) { r.SessionID = id }
}

func withEndTime(t time.Time) testRecordOpt {
	return func(r *RequestRecord) {
		r.EndTime = t
		if r.StartTime.After(t) || r.StartTime.Equal(t) {
			r.StartTime = t.Add(-1 * time.Second)
		}
	}
}

func withStartEnd(start, end time.Time) testRecordOpt {
	return func(r *RequestRecord) {
		r.StartTime = start
		r.EndTime = end
	}
}

func withTokens(input, output, cacheCreate, cacheRead int) testRecordOpt {
	return func(r *RequestRecord) {
		r.InputTokens = input
		r.OutputTokens = output
		r.CacheCreation = cacheCreate
		r.CacheRead = cacheRead
	}
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestNewTestRecord ./store`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add store/store_test.go
git commit -m "store: add newTestRecord helper for rolling-metric tests"
```

---

## Task 3: Implement `RollingITPM` (session-scoped)

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

Adds the first rolling-60-second metric method. End-time attribution: a record contributes if `end_time ∈ (now-60s, now]`. ITPM sum is `InputTokens + CacheCreation` (Anthropic's official definition; cache_read deliberately excluded).

- [ ] **Step 1: Write failing tests for `RollingITPM`**

Append to `store/store_test.go`:

```go
func TestRollingITPM_WindowBoundary(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// Record that ended 59s ago — should count
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-59*time.Second)),
		withTokens(1000, 0, 0, 0),
	))
	// Record that ended 61s ago — should NOT count
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-61*time.Second)),
		withTokens(9999, 0, 0, 0),
	))

	got := s.RollingITPM("s1", now)
	if got != 1000 {
		t.Fatalf("expected ITPM=1000, got %v", got)
	}
}

func TestRollingITPM_IncludesCacheCreationExcludesCacheRead(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-10*time.Second)),
		withTokens(500, 9999, 200, 100000), // input=500, output=9999 (ignored for ITPM), cache_creation=200, cache_read=100000 (excluded)
	))

	got := s.RollingITPM("s1", now)
	if got != 700 { // 500 + 200
		t.Fatalf("expected ITPM=700 (input+cache_creation), got %v", got)
	}
}

func TestRollingITPM_EmptySession(t *testing.T) {
	s := NewStore()
	got := s.RollingITPM("nonexistent", time.Now())
	if got != 0 {
		t.Fatalf("expected 0 for nonexistent session, got %v", got)
	}
}

func TestRollingITPM_AllRecordsOutsideWindow(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-5*time.Minute)),
		withTokens(5000, 1000, 0, 0),
	))
	got := s.RollingITPM("s1", now)
	if got != 0 {
		t.Fatalf("expected 0 for all-outside-window, got %v", got)
	}
}

func TestRollingITPM_InFlightExcluded(t *testing.T) {
	s := NewStore()
	s.AddInFlight("s1", "/v1/messages")
	got := s.RollingITPM("s1", time.Now())
	if got != 0 {
		t.Fatalf("expected 0 (in-flight excluded, no completed records), got %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestRollingITPM ./store`
Expected: compile error — `RollingITPM` undefined.

- [ ] **Step 3: Implement `RollingITPM`**

Add to `store/store.go` (below the existing `CalculateTPM` methods, near the bottom of the file):

```go
// RollingITPM returns the rolling 60-second ITPM (input tokens per minute) for a session.
// A record contributes if its EndTime falls in (now-60s, now].
// ITPM = Σ(InputTokens + CacheCreation) — matches Anthropic's documented ITPM definition.
// CacheRead is deliberately excluded (modern models do not count it toward ITPM).
// In-flight requests are excluded because their token counts are not yet known.
func (s *Store) RollingITPM(sessionID string, now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			total += r.InputTokens + r.CacheCreation
		}
	}
	return float64(total)
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestRollingITPM ./store`
Expected: PASS (all five tests).

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add RollingITPM with end-time attribution and cache_creation inclusion"
```

---

## Task 4: Implement `RollingOTPM` and `RollingRPM` (session-scoped)

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

- [ ] **Step 1: Write failing tests**

Append to `store/store_test.go`:

```go
func TestRollingOTPM_OutputOnly(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-10*time.Second)),
		withTokens(9999, 250, 9999, 9999), // only output (250) should count
	))

	got := s.RollingOTPM("s1", now)
	if got != 250 {
		t.Fatalf("expected OTPM=250 (output only), got %v", got)
	}
}

func TestRollingOTPM_WindowBoundary(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-59*time.Second)),
		withTokens(0, 100, 0, 0),
	))
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-61*time.Second)),
		withTokens(0, 9999, 0, 0),
	))
	got := s.RollingOTPM("s1", now)
	if got != 100 {
		t.Fatalf("expected OTPM=100, got %v", got)
	}
}

func TestRollingRPM_CountsRequests(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-10*time.Second)),
		withTokens(1000, 200, 0, 0),
	))
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-20*time.Second)),
		withTokens(0, 0, 0, 0), // zero-token error response still counts
	))
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-70*time.Second)), // outside window
		withTokens(1000, 200, 0, 0),
	))

	got := s.RollingRPM("s1", now)
	if got != 2 {
		t.Fatalf("expected RPM=2, got %d", got)
	}
}

func TestRollingRPM_EmptySession(t *testing.T) {
	s := NewStore()
	if s.RollingRPM("nope", time.Now()) != 0 {
		t.Fatal("expected 0 for empty session")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run "TestRollingOTPM|TestRollingRPM" ./store`
Expected: compile error — methods undefined.

- [ ] **Step 3: Implement `RollingOTPM` and `RollingRPM`**

Append to `store/store.go` (immediately after `RollingITPM`):

```go
// RollingOTPM returns the rolling 60-second OTPM (output tokens per minute) for a session.
// Uses end-time attribution. In-flight requests are excluded.
func (s *Store) RollingOTPM(sessionID string, now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			total += r.OutputTokens
		}
	}
	return float64(total)
}

// RollingRPM returns the rolling 60-second RPM (requests per minute) for a session.
// Counts all records (including zero-token error responses) whose EndTime falls in the window.
func (s *Store) RollingRPM(sessionID string, now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	count := 0
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			count++
		}
	}
	return count
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run "TestRollingOTPM|TestRollingRPM" ./store`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add RollingOTPM and RollingRPM"
```

---

## Task 5: Implement aggregate rolling variants (across all sessions)

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

- [ ] **Step 1: Write failing tests**

Append to `store/store_test.go`:

```go
func TestRollingAggregateITPM_SumsAllSessions(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-10*time.Second)),
		withTokens(1000, 200, 50, 0),
	))
	s.AddRecord(newTestRecord(
		withSession("s2"),
		withEndTime(now.Add(-20*time.Second)),
		withTokens(500, 100, 25, 0),
	))
	s.AddRecord(newTestRecord(
		withSession("s3"),
		withEndTime(now.Add(-70*time.Second)), // outside window
		withTokens(9999, 9999, 9999, 0),
	))

	got := s.RollingAggregateITPM(now)
	want := float64(1000 + 50 + 500 + 25) // 1575
	if got != want {
		t.Fatalf("expected aggregate ITPM=%v, got %v", want, got)
	}
}

func TestRollingAggregateOTPM(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-5*time.Second)),
		withTokens(0, 300, 0, 0),
	))
	s.AddRecord(newTestRecord(
		withSession("s2"),
		withEndTime(now.Add(-5*time.Second)),
		withTokens(0, 150, 0, 0),
	))
	got := s.RollingAggregateOTPM(now)
	if got != 450 {
		t.Fatalf("expected aggregate OTPM=450, got %v", got)
	}
}

func TestRollingAggregateRPM(t *testing.T) {
	s := NewStore()
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.AddRecord(newTestRecord(
			withSession("s1"),
			withEndTime(now.Add(time.Duration(-i*5)*time.Second)),
		))
	}
	for i := 0; i < 2; i++ {
		s.AddRecord(newTestRecord(
			withSession("s2"),
			withEndTime(now.Add(time.Duration(-i*5)*time.Second)),
		))
	}
	got := s.RollingAggregateRPM(now)
	if got != 5 {
		t.Fatalf("expected aggregate RPM=5, got %d", got)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestRollingAggregate ./store`
Expected: compile error.

- [ ] **Step 3: Implement the aggregate variants**

Append to `store/store.go` (after `RollingRPM`):

```go
// RollingAggregateITPM returns rolling 60s ITPM across all sessions in the store.
func (s *Store) RollingAggregateITPM(now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, sess := range s.sessions {
		for _, r := range sess.Requests {
			if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
				total += r.InputTokens + r.CacheCreation
			}
		}
	}
	return float64(total)
}

// RollingAggregateOTPM returns rolling 60s OTPM across all sessions.
func (s *Store) RollingAggregateOTPM(now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, sess := range s.sessions {
		for _, r := range sess.Requests {
			if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
				total += r.OutputTokens
			}
		}
	}
	return float64(total)
}

// RollingAggregateRPM returns rolling 60s RPM across all sessions.
func (s *Store) RollingAggregateRPM(now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	windowStart := now.Add(-60 * time.Second)
	count := 0
	for _, sess := range s.sessions {
		for _, r := range sess.Requests {
			if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
				count++
			}
		}
	}
	return count
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestRollingAggregate ./store`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add RollingAggregate{ITPM,OTPM,RPM} across all sessions"
```

---

## Task 6: Add `SessionPeaks` and per-session peak tracking

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

Attaches a `SessionPeaks` struct to each `Session`. Adds `UpdateSessionPeaks(sessionID, itpm, otpm, rpm, now)` that atomically compares-and-swaps under the store mutex. Adds `GetSessionPeaks` for the TUI to read.

- [ ] **Step 1: Write failing tests**

Append to `store/store_test.go`:

```go
func TestSessionPeaks_UpdateOnIncrease(t *testing.T) {
	s := NewStore()
	now := time.Now()
	// Need a session to exist before updating peaks
	s.AddRecord(newTestRecord(withSession("s1"), withEndTime(now)))

	s.UpdateSessionPeaks("s1", 100, 20, 3, now)
	peaks := s.GetSessionPeaks("s1")
	if peaks.MaxITPM != 100 || peaks.MaxOTPM != 20 || peaks.MaxRPM != 3 {
		t.Fatalf("expected (100,20,3), got (%v,%v,%d)", peaks.MaxITPM, peaks.MaxOTPM, peaks.MaxRPM)
	}

	// Higher ITPM — should update
	later := now.Add(time.Second)
	s.UpdateSessionPeaks("s1", 250, 10, 2, later)
	peaks = s.GetSessionPeaks("s1")
	if peaks.MaxITPM != 250 {
		t.Fatalf("expected MaxITPM=250, got %v", peaks.MaxITPM)
	}
	if !peaks.MaxITPMTime.Equal(later) {
		t.Fatalf("expected MaxITPMTime=%v, got %v", later, peaks.MaxITPMTime)
	}
	// OTPM not higher, so unchanged
	if peaks.MaxOTPM != 20 {
		t.Fatalf("expected MaxOTPM=20 (unchanged), got %v", peaks.MaxOTPM)
	}
}

func TestSessionPeaks_PersistAcrossQuiet(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.AddRecord(newTestRecord(withSession("s1"), withEndTime(now)))

	s.UpdateSessionPeaks("s1", 500, 100, 10, now)
	// Throughput drops to zero
	s.UpdateSessionPeaks("s1", 0, 0, 0, now.Add(10*time.Second))

	peaks := s.GetSessionPeaks("s1")
	if peaks.MaxITPM != 500 || peaks.MaxOTPM != 100 || peaks.MaxRPM != 10 {
		t.Fatalf("peaks should persist after quiet period, got %+v", peaks)
	}
}

func TestSessionPeaks_NonexistentSession(t *testing.T) {
	s := NewStore()
	peaks := s.GetSessionPeaks("nope")
	if peaks.MaxITPM != 0 || peaks.MaxOTPM != 0 || peaks.MaxRPM != 0 {
		t.Fatalf("expected zero peaks for nonexistent session, got %+v", peaks)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestSessionPeaks ./store`
Expected: compile error — `SessionPeaks`, `UpdateSessionPeaks`, `GetSessionPeaks` undefined.

- [ ] **Step 3: Define `SessionPeaks`, attach to `Session`, implement methods**

In `store/store.go`, modify the `Session` struct (around line 28) and add the `SessionPeaks` type above it:

```go
type SessionPeaks struct {
	MaxITPM     float64
	MaxITPMTime time.Time
	MaxOTPM     float64
	MaxOTPMTime time.Time
	MaxRPM      int
	MaxRPMTime  time.Time
}

type Session struct {
	ID        string
	StartTime time.Time
	LastSeen  time.Time
	Requests  []RequestRecord
	Peaks     SessionPeaks
}
```

Then add these methods near the bottom of `store/store.go` (after the aggregate rolling methods):

```go
// UpdateSessionPeaks compares the given current values against the session's stored peaks
// and updates each peak (and its timestamp) if exceeded. Safe to call from a single writer
// goroutine (e.g., the TUI tick). No-op if the session does not exist.
func (s *Store) UpdateSessionPeaks(sessionID string, itpm, otpm float64, rpm int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	if itpm > sess.Peaks.MaxITPM {
		sess.Peaks.MaxITPM = itpm
		sess.Peaks.MaxITPMTime = now
	}
	if otpm > sess.Peaks.MaxOTPM {
		sess.Peaks.MaxOTPM = otpm
		sess.Peaks.MaxOTPMTime = now
	}
	if rpm > sess.Peaks.MaxRPM {
		sess.Peaks.MaxRPM = rpm
		sess.Peaks.MaxRPMTime = now
	}
}

// GetSessionPeaks returns a copy of the session's peaks, or a zero-value SessionPeaks
// if the session does not exist.
func (s *Store) GetSessionPeaks(sessionID string) SessionPeaks {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return SessionPeaks{}
	}
	return sess.Peaks
}
```

Also update `GetSession` to include peaks in the returned copy — currently at around lines 85-97. The existing implementation copies `Requests` — peaks are a value type on `Session` so they will be copied automatically by `cp := *sess`. No change needed.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestSessionPeaks ./store`
Expected: PASS.

- [ ] **Step 5: Run the full store test suite**

Run: `go test ./store`
Expected: PASS (all tests including existing ones).

- [ ] **Step 6: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add SessionPeaks with compare-and-swap Update and Get methods"
```

---

## Task 7: Add aggregate peaks to `Store`

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

- [ ] **Step 1: Write failing test**

Append to `store/store_test.go`:

```go
func TestAggregatePeaks_TrackAcrossSessions(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// Start from zero
	peaks := s.GetAggregatePeaks()
	if peaks.MaxITPM != 0 || peaks.MaxOTPM != 0 || peaks.MaxRPM != 0 {
		t.Fatalf("expected zero peaks initially, got %+v", peaks)
	}

	s.UpdateAggregatePeaks(1000, 200, 15, now)
	peaks = s.GetAggregatePeaks()
	if peaks.MaxITPM != 1000 || peaks.MaxOTPM != 200 || peaks.MaxRPM != 15 {
		t.Fatalf("expected (1000,200,15), got %+v", peaks)
	}

	// Lower values — peaks should stay
	s.UpdateAggregatePeaks(500, 50, 5, now.Add(time.Second))
	peaks = s.GetAggregatePeaks()
	if peaks.MaxITPM != 1000 {
		t.Fatalf("expected MaxITPM=1000 unchanged, got %v", peaks.MaxITPM)
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test -run TestAggregatePeaks ./store`
Expected: compile error.

- [ ] **Step 3: Add the field and methods**

In `store/store.go`, update the `Store` struct (around line 41) to add the field:

```go
type Store struct {
	mu             sync.RWMutex
	sessions       map[string]*Session
	inflight       map[uint64]InFlightReq
	nextID         atomic.Uint64
	aggregatePeaks SessionPeaks
}
```

Then append these methods near the bottom of the file:

```go
// UpdateAggregatePeaks updates the store-level (across-all-sessions) peaks with the given
// current values. Mirrors UpdateSessionPeaks but at Store scope.
func (s *Store) UpdateAggregatePeaks(itpm, otpm float64, rpm int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itpm > s.aggregatePeaks.MaxITPM {
		s.aggregatePeaks.MaxITPM = itpm
		s.aggregatePeaks.MaxITPMTime = now
	}
	if otpm > s.aggregatePeaks.MaxOTPM {
		s.aggregatePeaks.MaxOTPM = otpm
		s.aggregatePeaks.MaxOTPMTime = now
	}
	if rpm > s.aggregatePeaks.MaxRPM {
		s.aggregatePeaks.MaxRPM = rpm
		s.aggregatePeaks.MaxRPMTime = now
	}
}

// GetAggregatePeaks returns a copy of the store-level peaks.
func (s *Store) GetAggregatePeaks() SessionPeaks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.aggregatePeaks
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -run TestAggregatePeaks ./store`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: add Store.aggregatePeaks with Update and Get methods"
```

---

## Task 8: Add concurrent-reader race test and remove old `CalculateTPM`

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

Removes the deprecated `CalculateTPM` and `CalculateAggregateTPM` methods (and their tests). Adds a concurrent-reader test to verify the new rolling methods are race-free.

- [ ] **Step 1: Write a concurrent-reader test**

Append to `store/store_test.go`:

```go
func TestRollingMetrics_ConcurrentReaders(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// Seed with some data
	for i := 0; i < 100; i++ {
		s.AddRecord(newTestRecord(
			withSession("s1"),
			withEndTime(now.Add(-time.Duration(i)*time.Second)),
			withTokens(100, 50, 10, 0),
		))
	}

	done := make(chan struct{})
	// 8 concurrent readers
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 1000; j++ {
				_ = s.RollingITPM("s1", time.Now())
				_ = s.RollingOTPM("s1", time.Now())
				_ = s.RollingRPM("s1", time.Now())
				_ = s.RollingAggregateITPM(time.Now())
				_ = s.GetSessionPeaks("s1")
			}
			done <- struct{}{}
		}()
	}
	// Concurrent writer
	go func() {
		for j := 0; j < 500; j++ {
			s.AddRecord(newTestRecord(withSession("s1"), withEndTime(time.Now())))
			s.UpdateSessionPeaks("s1", float64(j), float64(j), j, time.Now())
		}
		done <- struct{}{}
	}()

	for i := 0; i < 9; i++ {
		<-done
	}
}
```

- [ ] **Step 2: Run the test under -race to confirm it passes without data races**

Run: `go test -race -run TestRollingMetrics_ConcurrentReaders ./store`
Expected: PASS (no race warnings).

- [ ] **Step 3: Delete the old TPM tests**

From `store/store_test.go`, delete these four test functions entirely (previously at lines 147–213):
- `TestCalculateTPM`
- `TestCalculateTPMConcurrent`
- `TestCalculateTPMZeroActiveTime`
- `TestCalculateAggregateTPM`

- [ ] **Step 4: Delete the old TPM methods from `store/store.go`**

Remove these methods from `store/store.go` (currently at around lines 224–268):
- `CalculateTPM`
- `CalculateAggregateTPM`

Keep `collectIntervals`, `mergeIntervals`, `sumDurations`, and `GetActiveTime` — they are still used by the TUI's duration display.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./store`
Expected: PASS. If the TUI package references the deleted methods, the build of the whole module will fail — that will be fixed in Task 15. For now this task only verifies the store package alone builds and tests pass.

Verify with: `go test ./store` and `go vet ./store`. Both should be clean.

Note: `go build ./...` will FAIL at this point because `tui/tui.go` still calls `CalculateTPM` / `CalculateAggregateTPM`. That is expected; Task 15 removes those calls. Until then, only run per-package tests.

- [ ] **Step 6: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: remove obsolete CalculateTPM methods; add concurrent-reader race test"
```

---

## Task 9: Rewrite `extractRateLimitHeaders` in proxy for Anthropic header names

**Files:**
- Modify: `proxy/proxy.go:79-94`
- Modify: `proxy/proxy_test.go`

Replaces the broken `X-Ratelimit-*` scraping with correct `anthropic-ratelimit-*` parsing, and handles the OAuth `anthropic-ratelimit-unified-*` set. Returns a `RateLimitHeaders` struct.

- [ ] **Step 1: Write failing tests**

Append to `proxy/proxy_test.go`:

```go
func TestExtractRateLimitHeaders_AnthropicPerDirection(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("anthropic-ratelimit-input-tokens-limit", "450000")
	resp.Header.Set("anthropic-ratelimit-input-tokens-remaining", "448500")
	resp.Header.Set("anthropic-ratelimit-input-tokens-reset", "2026-04-05T14:30:00Z")
	resp.Header.Set("anthropic-ratelimit-output-tokens-limit", "90000")
	resp.Header.Set("anthropic-ratelimit-output-tokens-remaining", "89200")
	resp.Header.Set("anthropic-ratelimit-output-tokens-reset", "2026-04-05T14:30:00Z")
	resp.Header.Set("anthropic-ratelimit-requests-limit", "1000")
	resp.Header.Set("anthropic-ratelimit-requests-remaining", "998")
	resp.Header.Set("anthropic-ratelimit-requests-reset", "2026-04-05T14:30:00Z")
	resp.Header.Set("anthropic-ratelimit-tokens-remaining", "537700") // legacy combined
	resp.Header.Set("Retry-After", "")

	got := extractRateLimitHeaders(resp)
	if got.ITokensLimit == nil || *got.ITokensLimit != 450000 {
		t.Fatalf("ITokensLimit wrong: %v", got.ITokensLimit)
	}
	if got.ITokensRemaining == nil || *got.ITokensRemaining != 448500 {
		t.Fatalf("ITokensRemaining wrong: %v", got.ITokensRemaining)
	}
	if got.OTokensLimit == nil || *got.OTokensLimit != 90000 {
		t.Fatalf("OTokensLimit wrong: %v", got.OTokensLimit)
	}
	if got.RPMLimit == nil || *got.RPMLimit != 1000 {
		t.Fatalf("RPMLimit wrong: %v", got.RPMLimit)
	}
	if got.LegacyTokensRemaining == nil || *got.LegacyTokensRemaining != 537700 {
		t.Fatalf("LegacyTokensRemaining wrong: %v", got.LegacyTokensRemaining)
	}
	if got.RPMRemaining == nil || *got.RPMRemaining != 998 {
		t.Fatalf("RPMRemaining wrong: %v", got.RPMRemaining)
	}
}

func TestExtractRateLimitHeaders_UnifiedOAuth(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("anthropic-ratelimit-unified-status", "allowed")
	resp.Header.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "0.0184169696")
	resp.Header.Set("anthropic-ratelimit-unified-5h-reset", "1712345678")
	resp.Header.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	resp.Header.Set("anthropic-ratelimit-unified-7d-utilization", "0.7370692663")
	resp.Header.Set("anthropic-ratelimit-unified-7d-reset", "1712600000")
	resp.Header.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")

	got := extractRateLimitHeaders(resp)
	if got.UnifiedStatus != "allowed" {
		t.Fatalf("UnifiedStatus wrong: %q", got.UnifiedStatus)
	}
	if got.Unified5hUtil == nil || *got.Unified5hUtil < 0.018 || *got.Unified5hUtil > 0.019 {
		t.Fatalf("Unified5hUtil wrong: %v", got.Unified5hUtil)
	}
	if got.Unified5hReset == nil || *got.Unified5hReset != 1712345678 {
		t.Fatalf("Unified5hReset wrong: %v", got.Unified5hReset)
	}
	if got.Unified7dUtil == nil || *got.Unified7dUtil < 0.73 || *got.Unified7dUtil > 0.74 {
		t.Fatalf("Unified7dUtil wrong: %v", got.Unified7dUtil)
	}
	if got.UnifiedReprClaim != "five_hour" {
		t.Fatalf("UnifiedReprClaim wrong: %q", got.UnifiedReprClaim)
	}
}

func TestExtractRateLimitHeaders_AllAbsent(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	got := extractRateLimitHeaders(resp)
	if got.ITokensLimit != nil || got.OTokensRemaining != nil || got.Unified5hUtil != nil {
		t.Fatal("expected all nullable fields to be nil when headers absent")
	}
	if got.UnifiedStatus != "" || got.RetryAfter != "" {
		t.Fatal("expected all string fields to be empty when headers absent")
	}
}

func TestExtractRateLimitHeaders_MalformedUtilization(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "not-a-float")
	resp.Header.Set("anthropic-ratelimit-input-tokens-limit", "not-an-int")
	got := extractRateLimitHeaders(resp)
	if got.Unified5hUtil != nil {
		t.Fatal("expected nil for malformed utilization")
	}
	if got.ITokensLimit != nil {
		t.Fatal("expected nil for malformed int")
	}
}

func TestExtractRateLimitHeaders_UnifiedResetAsRFC3339(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("anthropic-ratelimit-unified-5h-reset", "2026-04-05T14:30:00Z")
	got := extractRateLimitHeaders(resp)
	if got.Unified5hReset == nil {
		t.Fatal("expected Unified5hReset to parse RFC3339 fallback")
	}
	// Should roughly be 2026-04-05 14:30 UTC as unix seconds
	expected := int64(1775737800)
	if *got.Unified5hReset != expected {
		t.Fatalf("expected %d, got %d", expected, *got.Unified5hReset)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestExtractRateLimitHeaders ./proxy`
Expected: compile error — `extractRateLimitHeaders` currently returns three scalars, not a struct.

- [ ] **Step 3: Rewrite `extractRateLimitHeaders`**

Replace the existing function in `proxy/proxy.go` (lines 79-94) with:

```go
// RateLimitHeaders holds parsed rate-limit data from an Anthropic API response.
// Pointer types are used for nullable numerics so "absent" is distinguishable from "zero".
type RateLimitHeaders struct {
	// Per-direction API-key headers (anthropic-ratelimit-{input,output}-tokens-*)
	ITokensLimit     *int
	ITokensRemaining *int
	ITokensReset     string
	OTokensLimit     *int
	OTokensRemaining *int
	OTokensReset     string
	RPMLimit         *int
	RPMRemaining     *int
	RPMReset         string

	// Legacy combined tokens header (anthropic-ratelimit-tokens-remaining) — Anthropic
	// still emits this as the "most restrictive tokens limit in effect" (combined
	// input+output when Workspace limits do not apply). Distinct from ITokensRemaining.
	// Requests have no direction, so anthropic-ratelimit-requests-remaining is read once
	// into RPMRemaining and no separate "legacy requests" field is needed.
	LegacyTokensRemaining *int

	// Unified OAuth headers (anthropic-ratelimit-unified-*)
	Unified5hUtil    *float64
	Unified5hReset   *int64 // unix seconds
	Unified5hStatus  string
	Unified7dUtil    *float64
	Unified7dReset   *int64
	Unified7dStatus  string
	UnifiedStatus    string
	UnifiedReprClaim string

	// Unchanged
	RetryAfter string
}

func extractRateLimitHeaders(resp *http.Response) RateLimitHeaders {
	h := resp.Header
	out := RateLimitHeaders{
		RetryAfter: h.Get("Retry-After"),
	}

	// Per-direction API-key headers
	out.ITokensLimit = parseIntHeader(h, "anthropic-ratelimit-input-tokens-limit")
	out.ITokensRemaining = parseIntHeader(h, "anthropic-ratelimit-input-tokens-remaining")
	out.ITokensReset = h.Get("anthropic-ratelimit-input-tokens-reset")
	out.OTokensLimit = parseIntHeader(h, "anthropic-ratelimit-output-tokens-limit")
	out.OTokensRemaining = parseIntHeader(h, "anthropic-ratelimit-output-tokens-remaining")
	out.OTokensReset = h.Get("anthropic-ratelimit-output-tokens-reset")
	out.RPMLimit = parseIntHeader(h, "anthropic-ratelimit-requests-limit")
	out.RPMRemaining = parseIntHeader(h, "anthropic-ratelimit-requests-remaining")
	out.RPMReset = h.Get("anthropic-ratelimit-requests-reset")

	// Legacy combined tokens header. If absent, fall back to ITokensRemaining so the
	// legacy ratelimit_tok_remaining DB column still reports something useful.
	out.LegacyTokensRemaining = parseIntHeader(h, "anthropic-ratelimit-tokens-remaining")
	if out.LegacyTokensRemaining == nil && out.ITokensRemaining != nil {
		v := *out.ITokensRemaining
		out.LegacyTokensRemaining = &v
	}

	// Unified OAuth headers
	out.UnifiedStatus = h.Get("anthropic-ratelimit-unified-status")
	out.UnifiedReprClaim = h.Get("anthropic-ratelimit-unified-representative-claim")
	out.Unified5hStatus = h.Get("anthropic-ratelimit-unified-5h-status")
	out.Unified5hUtil = parseFloatHeader(h, "anthropic-ratelimit-unified-5h-utilization")
	out.Unified5hReset = parseUnixOrRFC3339(h, "anthropic-ratelimit-unified-5h-reset")
	out.Unified7dStatus = h.Get("anthropic-ratelimit-unified-7d-status")
	out.Unified7dUtil = parseFloatHeader(h, "anthropic-ratelimit-unified-7d-utilization")
	out.Unified7dReset = parseUnixOrRFC3339(h, "anthropic-ratelimit-unified-7d-reset")

	return out
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
```

You will also need to update the import block at the top of `proxy/proxy.go` if `time` and `strconv` are not already imported (they should be — verify by checking the existing imports at lines 3-18 of the current file).

Note: `modifyResponse` currently uses the three scalar return values from `extractRateLimitHeaders`. It will break temporarily — next task fixes it.

- [ ] **Step 4: Update `modifyResponse` call sites to compile (temporary shim)**

In `proxy/proxy.go`, `modifyResponse` has two places that call the old signature `extractRateLimitHeaders(resp)` returning `(retryAfter, reqRemaining, tokRemaining)`:

- Line 128 in the existing file.
- It's used in the error-path record (lines 130-148) and the success-path record (lines 128, 204-226).

Update the calls temporarily to preserve compilation — we'll plumb the full fields onto `RequestRecord` in Task 10. For now, extract the legacy values from the new struct:

Replace the single call line in `modifyResponse`:

```go
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
```

This preserves the two old sentinel-based local variables. The rest of `modifyResponse` (which uses these locals to populate `RequestRecord.RateLimitReqRemaining` and `RateLimitTokRemaining`) works unchanged.

- [ ] **Step 5: Run proxy tests to confirm they pass**

Run: `go test ./proxy`
Expected: PASS (new tests and existing ones).

- [ ] **Step 6: Commit**

```bash
git add proxy/proxy.go proxy/proxy_test.go
git commit -m "proxy: rewrite extractRateLimitHeaders for anthropic-ratelimit-* and unified-* headers"
```

---

## Task 10: Plumb all rate-limit header fields into `RequestRecord`

**Files:**
- Modify: `proxy/proxy.go` (`modifyResponse`)

Extends the two `RequestRecord` construction sites in `modifyResponse` (error path ~line 131 and success path ~line 204) to populate all the new fields from `rateHeaders`.

- [ ] **Step 1: Write a failing integration test**

Append to `proxy/proxy_test.go`:

```go
func TestModifyResponsePopulatesAllHeaderFields(t *testing.T) {
	// This test exercises modifyResponse directly with a synthetic response to verify
	// all new header fields land on the RequestRecord submitted to the store and DB channel.
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
	// Drain and read the body so the parser goroutine completes
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Wait for goroutine to submit the record
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
```

Add to `proxy/proxy_test.go` imports if missing:
```go
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
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test -run TestModifyResponsePopulatesAllHeaderFields ./proxy`
Expected: FAIL — the header fields are not yet copied onto `RequestRecord` in `modifyResponse`.

- [ ] **Step 3: Plumb the fields in both code paths**

In `proxy/proxy.go`, add a helper function near the other helpers (below `parseUnixOrRFC3339`):

```go
// applyRateLimitHeaders copies all parsed rate-limit header fields from h onto rec.
// The legacy scalar column RateLimitReqRemaining is populated from RPMRemaining
// (Anthropic has a single requests-remaining header — requests have no direction).
// RateLimitTokRemaining is populated from LegacyTokensRemaining, which reads the
// legacy combined anthropic-ratelimit-tokens-remaining header. -1 is the absent sentinel.
func applyRateLimitHeaders(rec *store.RequestRecord, h RateLimitHeaders) {
	rec.RetryAfter = h.RetryAfter
	rec.RateLimitReqRemaining = -1
	if h.RPMRemaining != nil {
		rec.RateLimitReqRemaining = *h.RPMRemaining
	}
	rec.RateLimitTokRemaining = -1
	if h.LegacyTokensRemaining != nil {
		rec.RateLimitTokRemaining = *h.LegacyTokensRemaining
	}

	rec.ITokensLimit = h.ITokensLimit
	rec.ITokensRemaining = h.ITokensRemaining
	rec.ITokensReset = h.ITokensReset
	rec.OTokensLimit = h.OTokensLimit
	rec.OTokensRemaining = h.OTokensRemaining
	rec.OTokensReset = h.OTokensReset
	rec.RPMLimit = h.RPMLimit
	rec.RPMRemaining = h.RPMRemaining
	rec.RPMReset = h.RPMReset

	rec.Unified5hUtil = h.Unified5hUtil
	rec.Unified5hReset = h.Unified5hReset
	rec.Unified5hStatus = h.Unified5hStatus
	rec.Unified7dUtil = h.Unified7dUtil
	rec.Unified7dReset = h.Unified7dReset
	rec.Unified7dStatus = h.Unified7dStatus
	rec.UnifiedStatus = h.UnifiedStatus
	rec.UnifiedReprClaim = h.UnifiedReprClaim
}
```

Now update `modifyResponse` to use it. Replace the temporary shim from Task 9:

```go
	rateHeaders := extractRateLimitHeaders(resp)
```

(remove the three `retryAfter` / `reqRemaining` / `tokRemaining` local variables)

Then in the **error path** (around line 131), replace the `rec := store.RequestRecord{...}` block with:

```go
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rec := store.RequestRecord{
			StartTime:  requestStart,
			EndTime:    time.Now(),
			Endpoint:   endpoint,
			StatusCode: resp.StatusCode,
			SessionID:  sessionID,
		}
		applyRateLimitHeaders(&rec, rateHeaders)
		cfg.Store.AddRecord(rec)
		select {
		case cfg.DBChan <- rec:
		default:
			log.Println("warning: DB write channel full, record dropped")
		}
		return nil
	}
```

In the **gzip-error path** (around line 169), similarly:

```go
				rec := store.RequestRecord{
					StartTime:  requestStart,
					EndTime:    time.Now(),
					Endpoint:   endpoint,
					StatusCode: resp.StatusCode,
					HasError:   true,
					SessionID:  sessionID,
				}
				applyRateLimitHeaders(&rec, rateHeaders)
				cfg.Store.AddRecord(rec)
				cfg.DBChan <- rec
				return
```

And in the **success path** (around line 204):

```go
		rec := store.RequestRecord{
			StartTime:     requestStart,
			EndTime:       endTime,
			Model:         result.Model,
			InputTokens:   result.InputTokens,
			OutputTokens:  result.OutputTokens,
			CacheCreation: result.CacheCreation,
			CacheRead:     result.CacheRead,
			Endpoint:      endpoint,
			StatusCode:    resp.StatusCode,
			TTFT:          trc.TTFT(),
			HasError:      result.HasError,
			SessionID:     sessionID,
		}
		applyRateLimitHeaders(&rec, rateHeaders)
		cfg.Store.AddRecord(rec)
		select {
		case cfg.DBChan <- rec:
		default:
			log.Println("warning: DB write channel full, record dropped")
		}
```

- [ ] **Step 4: Run proxy tests**

Run: `go test ./proxy`
Expected: PASS (all tests).

- [ ] **Step 5: Run with -race**

Run: `go test -race ./proxy`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add proxy/proxy.go proxy/proxy_test.go
git commit -m "proxy: plumb all rate-limit header fields onto RequestRecord"
```

---

## Task 11: DB schema migration — idempotent `ALTER TABLE ADD COLUMN`

**Files:**
- Modify: `db/db.go:49-74` (`createSchema`)
- Modify: `db/db_test.go`

Adds 17 nullable columns via `ALTER TABLE ADD COLUMN`, wrapped in duplicate-column error tolerance so repeated startup is idempotent.

- [ ] **Step 1: Write failing tests**

Append to `db/db_test.go`:

```go
func TestSchemaMigration_AddsNewColumns(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rows, err := d.conn.Query("PRAGMA table_info(requests)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	expected := []string{
		"itokens_limit", "itokens_remaining", "itokens_reset",
		"otokens_limit", "otokens_remaining", "otokens_reset",
		"rpm_limit", "rpm_remaining", "rpm_reset",
		"unified_5h_util", "unified_5h_reset", "unified_5h_status",
		"unified_7d_util", "unified_7d_reset", "unified_7d_status",
		"unified_status", "unified_repr_claim",
	}
	for _, c := range expected {
		if !cols[c] {
			t.Errorf("missing column: %s", c)
		}
	}
}

func TestSchemaMigration_Idempotent(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Run createSchema a second time — should not error
	if err := d.createSchema(); err != nil {
		t.Fatalf("second createSchema call failed: %v", err)
	}
}
```

Add `"database/sql"` to imports if not already present in `db/db_test.go` (it's needed for `sql.NullString`).

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestSchemaMigration ./db`
Expected: `TestSchemaMigration_AddsNewColumns` fails because new columns don't exist. `TestSchemaMigration_Idempotent` should pass already (nothing being altered).

- [ ] **Step 3: Update `createSchema` to add the new columns**

Replace the `createSchema` method in `db/db.go` (currently lines 49-74) with:

```go
func (d *DB) createSchema() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS requests (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id    TEXT NOT NULL,
			start_time    DATETIME NOT NULL,
			end_time      DATETIME NOT NULL,
			model         TEXT NOT NULL DEFAULT '',
			endpoint      TEXT NOT NULL DEFAULT '',
			status_code   INTEGER NOT NULL,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation INTEGER NOT NULL DEFAULT 0,
			cache_read    INTEGER NOT NULL DEFAULT 0,
			ttft_ms       INTEGER NOT NULL DEFAULT 0,
			retry_after   TEXT NOT NULL DEFAULT '',
			ratelimit_req_remaining INTEGER NOT NULL DEFAULT -1,
			ratelimit_tok_remaining INTEGER NOT NULL DEFAULT -1,
			has_error     INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_requests_session_time ON requests(session_id, start_time);
		CREATE INDEX IF NOT EXISTS idx_requests_start_time ON requests(start_time);
		CREATE INDEX IF NOT EXISTS idx_requests_status_code_time ON requests(status_code, start_time);
	`)
	if err != nil {
		return err
	}

	// Additive migration for the ITPM/OTPM/RPM design. Each ALTER TABLE is idempotent
	// via duplicate-column error tolerance so repeated startup is safe.
	alters := []string{
		`ALTER TABLE requests ADD COLUMN itokens_limit       INTEGER`,
		`ALTER TABLE requests ADD COLUMN itokens_remaining   INTEGER`,
		`ALTER TABLE requests ADD COLUMN itokens_reset       TEXT`,
		`ALTER TABLE requests ADD COLUMN otokens_limit       INTEGER`,
		`ALTER TABLE requests ADD COLUMN otokens_remaining   INTEGER`,
		`ALTER TABLE requests ADD COLUMN otokens_reset       TEXT`,
		`ALTER TABLE requests ADD COLUMN rpm_limit           INTEGER`,
		`ALTER TABLE requests ADD COLUMN rpm_remaining       INTEGER`,
		`ALTER TABLE requests ADD COLUMN rpm_reset           TEXT`,
		`ALTER TABLE requests ADD COLUMN unified_5h_util     REAL`,
		`ALTER TABLE requests ADD COLUMN unified_5h_reset    INTEGER`,
		`ALTER TABLE requests ADD COLUMN unified_5h_status   TEXT`,
		`ALTER TABLE requests ADD COLUMN unified_7d_util     REAL`,
		`ALTER TABLE requests ADD COLUMN unified_7d_reset    INTEGER`,
		`ALTER TABLE requests ADD COLUMN unified_7d_status   TEXT`,
		`ALTER TABLE requests ADD COLUMN unified_status      TEXT`,
		`ALTER TABLE requests ADD COLUMN unified_repr_claim  TEXT`,
	}
	for _, stmt := range alters {
		if _, err := d.conn.Exec(stmt); err != nil {
			// SQLite returns "duplicate column name: X" if the column exists. Ignore that.
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("migration failed (%s): %w", stmt, err)
			}
		}
	}
	return nil
}
```

Add `"strings"` to the imports at the top of `db/db.go` if not already present.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestSchemaMigration ./db`
Expected: PASS.

- [ ] **Step 5: Run the full db test suite to catch regressions**

Run: `go test ./db`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add db/db.go db/db_test.go
git commit -m "db: additive ALTER TABLE migration for rate-limit header columns"
```

---

## Task 12: Update `InsertRecord` and `scanRecords` for new nullable columns

**Files:**
- Modify: `db/db.go` (`InsertRecord`, `scanRecords`, `QueryRequests`, `QueryThrottle`)
- Modify: `db/db_test.go`

Wires the new columns through the write and read paths. Uses `sql.NullInt64` / `sql.NullFloat64` / `sql.NullString` on the scan side to handle NULLs.

- [ ] **Step 1: Write a failing round-trip test**

Append to `db/db_test.go`:

```go
func TestInsertAndQueryRecord_WithHeaderFields(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	iLimit := 450000
	iRemaining := 448500
	fiveHUtil := 0.0184
	fiveHReset := int64(1712345678)

	now := time.Now()
	rec := store.RequestRecord{
		SessionID:        "s1",
		StartTime:        now.Add(-5 * time.Second),
		EndTime:          now,
		Model:            "claude-sonnet-4",
		StatusCode:       200,
		InputTokens:      1000,
		OutputTokens:     200,
		ITokensLimit:     &iLimit,
		ITokensRemaining: &iRemaining,
		ITokensReset:     "2026-04-05T14:30:00Z",
		Unified5hUtil:    &fiveHUtil,
		Unified5hReset:   &fiveHReset,
		UnifiedStatus:    "allowed",
		UnifiedReprClaim: "five_hour",
	}
	if err := d.InsertRecord(rec); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	rows, err := d.QueryRequests("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.ITokensLimit == nil || *got.ITokensLimit != 450000 {
		t.Errorf("ITokensLimit wrong: %v", got.ITokensLimit)
	}
	if got.Unified5hUtil == nil || *got.Unified5hUtil != 0.0184 {
		t.Errorf("Unified5hUtil wrong: %v", got.Unified5hUtil)
	}
	if got.UnifiedStatus != "allowed" {
		t.Errorf("UnifiedStatus wrong: %q", got.UnifiedStatus)
	}
}

func TestInsertAndQueryRecord_AllHeadersNil(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec := store.RequestRecord{
		SessionID: "s1",
		StartTime: time.Now().Add(-1 * time.Second),
		EndTime:   time.Now(),
		Model:     "m",
		StatusCode: 200,
	}
	if err := d.InsertRecord(rec); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	rows, err := d.QueryRequests("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.ITokensLimit != nil {
		t.Errorf("expected nil ITokensLimit, got %v", *got.ITokensLimit)
	}
	if got.Unified5hUtil != nil {
		t.Errorf("expected nil Unified5hUtil, got %v", *got.Unified5hUtil)
	}
	if got.UnifiedStatus != "" {
		t.Errorf("expected empty UnifiedStatus, got %q", got.UnifiedStatus)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run "TestInsertAndQueryRecord_With|TestInsertAndQueryRecord_All" ./db`
Expected: FAIL — insert/scan doesn't touch the new columns yet.

- [ ] **Step 3: Update `InsertRecord` to write new columns**

Replace `InsertRecord` in `db/db.go` with:

```go
func (d *DB) InsertRecord(rec store.RequestRecord) error {
	_, err := d.conn.Exec(`
		INSERT INTO requests (
			session_id, start_time, end_time, model, endpoint, status_code,
			input_tokens, output_tokens, cache_creation, cache_read,
			ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error,
			itokens_limit, itokens_remaining, itokens_reset,
			otokens_limit, otokens_remaining, otokens_reset,
			rpm_limit, rpm_remaining, rpm_reset,
			unified_5h_util, unified_5h_reset, unified_5h_status,
			unified_7d_util, unified_7d_reset, unified_7d_status,
			unified_status, unified_repr_claim
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		          ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SessionID, rec.StartTime.UTC(), rec.EndTime.UTC(), rec.Model, rec.Endpoint, rec.StatusCode,
		rec.InputTokens, rec.OutputTokens, rec.CacheCreation, rec.CacheRead,
		rec.TTFT.Milliseconds(), rec.RetryAfter, rec.RateLimitReqRemaining, rec.RateLimitTokRemaining,
		boolToInt(rec.HasError),
		intPtrToNullInt(rec.ITokensLimit), intPtrToNullInt(rec.ITokensRemaining), stringToNullString(rec.ITokensReset),
		intPtrToNullInt(rec.OTokensLimit), intPtrToNullInt(rec.OTokensRemaining), stringToNullString(rec.OTokensReset),
		intPtrToNullInt(rec.RPMLimit), intPtrToNullInt(rec.RPMRemaining), stringToNullString(rec.RPMReset),
		floatPtrToNullFloat(rec.Unified5hUtil), int64PtrToNullInt(rec.Unified5hReset), stringToNullString(rec.Unified5hStatus),
		floatPtrToNullFloat(rec.Unified7dUtil), int64PtrToNullInt(rec.Unified7dReset), stringToNullString(rec.Unified7dStatus),
		stringToNullString(rec.UnifiedStatus), stringToNullString(rec.UnifiedReprClaim),
	)
	return err
}

func intPtrToNullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func int64PtrToNullInt(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func floatPtrToNullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
```

- [ ] **Step 4: Update `scanRecords` and its SELECT statements to read new columns**

Find all three SELECT statements in `db/db.go` (`QueryRequests` at ~line 99, `QueryThrottle` at ~line 162, and the `scanRecords` receiver) and extend each to include the new columns.

Replace `QueryRequests` SELECT column list:

```go
func (d *DB) QueryRequests(from, to, sessionID string, limit int) ([]store.RequestRecord, error) {
	q := `SELECT session_id, start_time, end_time, model, endpoint, status_code,
		input_tokens, output_tokens, cache_creation, cache_read,
		ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error,
		itokens_limit, itokens_remaining, itokens_reset,
		otokens_limit, otokens_remaining, otokens_reset,
		rpm_limit, rpm_remaining, rpm_reset,
		unified_5h_util, unified_5h_reset, unified_5h_status,
		unified_7d_util, unified_7d_reset, unified_7d_status,
		unified_status, unified_repr_claim
		FROM requests WHERE 1=1`
	var args []any
	if from != "" {
		q += " AND start_time >= ?"
		args = append(args, from)
	}
	if to != "" {
		q += " AND start_time <= ?"
		args = append(args, to)
	}
	if sessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sessionID)
	}
	q += " ORDER BY start_time DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}
```

Replace `QueryThrottle` SELECT column list (use identical column list as `QueryRequests`):

```go
func (d *DB) QueryThrottle(ttftThresholdMs int, from, to, sessionID string, limit int) ([]store.RequestRecord, error) {
	q := `SELECT session_id, start_time, end_time, model, endpoint, status_code,
		input_tokens, output_tokens, cache_creation, cache_read,
		ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error,
		itokens_limit, itokens_remaining, itokens_reset,
		otokens_limit, otokens_remaining, otokens_reset,
		rpm_limit, rpm_remaining, rpm_reset,
		unified_5h_util, unified_5h_reset, unified_5h_status,
		unified_7d_util, unified_7d_reset, unified_7d_status,
		unified_status, unified_repr_claim
		FROM requests WHERE (status_code IN (429, 529) OR has_error = 1 OR ttft_ms > ?)`
	args := []any{ttftThresholdMs}
	if from != "" {
		q += " AND start_time >= ?"
		args = append(args, from)
	}
	if to != "" {
		q += " AND start_time <= ?"
		args = append(args, to)
	}
	if sessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sessionID)
	}
	q += " ORDER BY start_time DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}
```

Replace `scanRecords` (currently at lines 247-268):

```go
func scanRecords(rows *sql.Rows) ([]store.RequestRecord, error) {
	var records []store.RequestRecord
	for rows.Next() {
		var r store.RequestRecord
		var ttftMs int64
		var hasErr int
		var startTime, endTime string
		var iLimit, iRemaining, oLimit, oRemaining, rLimit, rRemaining sql.NullInt64
		var iReset, oReset, rReset sql.NullString
		var u5hUtil, u7dUtil sql.NullFloat64
		var u5hReset, u7dReset sql.NullInt64
		var u5hStatus, u7dStatus, uStatus, uReprClaim sql.NullString

		if err := rows.Scan(
			&r.SessionID, &startTime, &endTime, &r.Model, &r.Endpoint, &r.StatusCode,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreation, &r.CacheRead,
			&ttftMs, &r.RetryAfter, &r.RateLimitReqRemaining, &r.RateLimitTokRemaining, &hasErr,
			&iLimit, &iRemaining, &iReset,
			&oLimit, &oRemaining, &oReset,
			&rLimit, &rRemaining, &rReset,
			&u5hUtil, &u5hReset, &u5hStatus,
			&u7dUtil, &u7dReset, &u7dStatus,
			&uStatus, &uReprClaim,
		); err != nil {
			return nil, err
		}
		r.StartTime = parseTime(startTime)
		r.EndTime = parseTime(endTime)
		r.TTFT = time.Duration(ttftMs) * time.Millisecond
		r.HasError = hasErr != 0

		r.ITokensLimit = nullIntToIntPtr(iLimit)
		r.ITokensRemaining = nullIntToIntPtr(iRemaining)
		r.ITokensReset = iReset.String
		r.OTokensLimit = nullIntToIntPtr(oLimit)
		r.OTokensRemaining = nullIntToIntPtr(oRemaining)
		r.OTokensReset = oReset.String
		r.RPMLimit = nullIntToIntPtr(rLimit)
		r.RPMRemaining = nullIntToIntPtr(rRemaining)
		r.RPMReset = rReset.String
		r.Unified5hUtil = nullFloatToFloatPtr(u5hUtil)
		r.Unified5hReset = nullIntToInt64Ptr(u5hReset)
		r.Unified5hStatus = u5hStatus.String
		r.Unified7dUtil = nullFloatToFloatPtr(u7dUtil)
		r.Unified7dReset = nullIntToInt64Ptr(u7dReset)
		r.Unified7dStatus = u7dStatus.String
		r.UnifiedStatus = uStatus.String
		r.UnifiedReprClaim = uReprClaim.String

		records = append(records, r)
	}
	return records, rows.Err()
}

func nullIntToIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullIntToInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullFloatToFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `go test ./db`
Expected: PASS — new round-trip tests and existing ones.

- [ ] **Step 6: Commit**

```bash
git add db/db.go db/db_test.go
git commit -m "db: round-trip new nullable rate-limit header columns via InsertRecord/scanRecords"
```

---

## Task 13: Implement `QueryTPMBuckets` and `QueryTPMPeak`

**Files:**
- Modify: `db/db.go`
- Modify: `db/db_test.go`

Adds the two SQL methods powering the new `--query tpm` command.

- [ ] **Step 1: Write failing tests**

Append to `db/db_test.go`:

```go
func TestQueryTPMBuckets_BasicShape(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert 3 records in minute N (two with tokens), 2 records in minute N+1
	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	recs := []store.RequestRecord{
		{SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second), InputTokens: 100, OutputTokens: 20, StatusCode: 200},
		{SessionID: "s1", StartTime: base, EndTime: base.Add(10 * time.Second), InputTokens: 200, OutputTokens: 30, CacheCreation: 50, StatusCode: 200},
		{SessionID: "s1", StartTime: base, EndTime: base.Add(30 * time.Second), InputTokens: 0, OutputTokens: 0, StatusCode: 500}, // error, still counts
		{SessionID: "s1", StartTime: base.Add(65 * time.Second), EndTime: base.Add(70 * time.Second), InputTokens: 500, OutputTokens: 100, StatusCode: 200},
		{SessionID: "s1", StartTime: base.Add(75 * time.Second), EndTime: base.Add(80 * time.Second), InputTokens: 0, OutputTokens: 200, StatusCode: 200},
	}
	for _, r := range recs {
		if err := d.InsertRecord(r); err != nil {
			t.Fatal(err)
		}
	}

	// 1-minute buckets
	from := base.Add(-1 * time.Second)
	to := base.Add(5 * time.Minute)
	buckets, err := d.QueryTPMBuckets(from, to, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}

	// Minute N: ITPM = 100 + 200 + 50 = 350, OTPM = 50, RPM = 3
	if buckets[0].MaxITPM != 350 {
		t.Errorf("bucket[0] MaxITPM: want 350, got %v", buckets[0].MaxITPM)
	}
	if buckets[0].MaxOTPM != 50 {
		t.Errorf("bucket[0] MaxOTPM: want 50, got %v", buckets[0].MaxOTPM)
	}
	if buckets[0].MaxRPM != 3 {
		t.Errorf("bucket[0] MaxRPM: want 3, got %d", buckets[0].MaxRPM)
	}

	// Minute N+1: ITPM = 500, OTPM = 300, RPM = 2
	if buckets[1].MaxITPM != 500 {
		t.Errorf("bucket[1] MaxITPM: want 500, got %v", buckets[1].MaxITPM)
	}
}

func TestQueryTPMBuckets_EmptyRange(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	buckets, err := d.QueryTPMBuckets(time.Now().Add(-1*time.Hour), time.Now(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Fatalf("expected empty result, got %d buckets", len(buckets))
	}
}

func TestQueryTPMPeak_AllTime(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second), InputTokens: 100, OutputTokens: 20, StatusCode: 200})
	d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: base.Add(90 * time.Second), EndTime: base.Add(95 * time.Second), InputTokens: 500, OutputTokens: 200, StatusCode: 200})
	d.InsertRecord(store.RequestRecord{SessionID: "s2", StartTime: base.Add(180 * time.Second), EndTime: base.Add(185 * time.Second), InputTokens: 300, OutputTokens: 50, StatusCode: 200})

	peak, err := d.QueryTPMPeak(base.Add(-1*time.Minute), base.Add(10*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(peak) != 1 {
		t.Fatalf("expected 1 peak row (all-time), got %d", len(peak))
	}
	if peak[0].MaxITPM != 500 {
		t.Errorf("MaxITPM: want 500, got %v", peak[0].MaxITPM)
	}
	if peak[0].MaxOTPM != 200 {
		t.Errorf("MaxOTPM: want 200, got %v", peak[0].MaxOTPM)
	}
}

func TestQueryTPMPeak_GroupBySession(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second), InputTokens: 100, OutputTokens: 20, StatusCode: 200})
	d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: base.Add(90 * time.Second), EndTime: base.Add(95 * time.Second), InputTokens: 500, OutputTokens: 200, StatusCode: 200})
	d.InsertRecord(store.RequestRecord{SessionID: "s2", StartTime: base.Add(180 * time.Second), EndTime: base.Add(185 * time.Second), InputTokens: 300, OutputTokens: 50, StatusCode: 200})

	peak, err := d.QueryTPMPeak(base.Add(-1*time.Minute), base.Add(10*time.Minute), "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(peak) != 2 {
		t.Fatalf("expected 2 peak rows (one per session), got %d", len(peak))
	}
	byID := map[string]TPMPeak{}
	for _, p := range peak {
		byID[p.SessionID] = p
	}
	if byID["s1"].MaxITPM != 500 {
		t.Errorf("s1 MaxITPM: want 500, got %v", byID["s1"].MaxITPM)
	}
	if byID["s2"].MaxITPM != 300 {
		t.Errorf("s2 MaxITPM: want 300, got %v", byID["s2"].MaxITPM)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run "TestQueryTPM" ./db`
Expected: compile error — `QueryTPMBuckets`, `QueryTPMPeak`, `TPMBucket`, `TPMPeak` undefined.

- [ ] **Step 3: Implement the result types and methods**

Append to `db/db.go`:

```go
// TPMBucket is one row of the bucketed TPM query result.
type TPMBucket struct {
	BucketStart   time.Time // start of the bucket in UTC
	MaxITPM       float64
	MaxOTPM       float64
	MaxRPM        int
	SampleMinutes int // count of minute windows with data in this bucket
}

// TPMPeak is one row of the peak TPM query result.
// For all-time peak (no group-by), SessionID is empty and one row is returned.
// For group-by session, one row per session_id is returned.
type TPMPeak struct {
	SessionID    string
	MaxITPM      float64
	MaxITPMTime  time.Time
	MaxOTPM      float64
	MaxOTPMTime  time.Time
	MaxRPM       int
	MaxRPMTime   time.Time
	FirstSeen    time.Time
	LastSeen     time.Time
}

// QueryTPMBuckets returns bucketed max-per-bucket ITPM/OTPM/RPM over the time range.
// The inner aggregation groups by minute-of-end; the outer query groups those minutes
// into the requested bucket size and picks the peak per bucket.
func (d *DB) QueryTPMBuckets(from, to time.Time, bucketSeconds int) ([]TPMBucket, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 60
	}
	q := `
		WITH minute_windows AS (
			SELECT
				CAST(strftime('%s', end_time) AS INTEGER) / 60 * 60 AS minute_epoch,
				SUM(input_tokens + cache_creation) AS itpm,
				SUM(output_tokens)                 AS otpm,
				COUNT(*)                           AS rpm
			FROM requests
			WHERE end_time BETWEEN ? AND ?
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
		ORDER BY bucket_epoch ASC`
	rows, err := d.conn.Query(q, from.UTC(), to.UTC(), bucketSeconds, bucketSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TPMBucket
	for rows.Next() {
		var bucketEpoch int64
		var maxITPM, maxOTPM sql.NullFloat64
		var maxRPM sql.NullInt64
		var samples int
		if err := rows.Scan(&bucketEpoch, &maxITPM, &maxOTPM, &maxRPM, &samples); err != nil {
			return nil, err
		}
		b := TPMBucket{
			BucketStart:   time.Unix(bucketEpoch, 0).UTC(),
			SampleMinutes: samples,
		}
		if maxITPM.Valid {
			b.MaxITPM = maxITPM.Float64
		}
		if maxOTPM.Valid {
			b.MaxOTPM = maxOTPM.Float64
		}
		if maxRPM.Valid {
			b.MaxRPM = int(maxRPM.Int64)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QueryTPMPeak returns peak ITPM/OTPM/RPM over the time range. With groupBy="session",
// returns one row per session. Otherwise returns a single row for the all-time peak.
func (d *DB) QueryTPMPeak(from, to time.Time, groupBy string) ([]TPMPeak, error) {
	if groupBy != "" && groupBy != "session" {
		return nil, fmt.Errorf("unknown group-by: %q (valid: \"\", \"session\")", groupBy)
	}

	// Compute per-minute rates first, then pick max per grouping.
	var q string
	if groupBy == "session" {
		q = `
			WITH minute_windows AS (
				SELECT
					session_id,
					CAST(strftime('%s', end_time) AS INTEGER) / 60 * 60 AS minute_epoch,
					SUM(input_tokens + cache_creation) AS itpm,
					SUM(output_tokens)                 AS otpm,
					COUNT(*)                           AS rpm
				FROM requests
				WHERE end_time BETWEEN ? AND ?
				GROUP BY session_id, minute_epoch
			)
			SELECT
				session_id,
				MAX(itpm),
				MAX(otpm),
				MAX(rpm),
				MIN(minute_epoch),
				MAX(minute_epoch)
			FROM minute_windows
			GROUP BY session_id
			ORDER BY MAX(itpm) DESC`
	} else {
		q = `
			WITH minute_windows AS (
				SELECT
					session_id,
					CAST(strftime('%s', end_time) AS INTEGER) / 60 * 60 AS minute_epoch,
					SUM(input_tokens + cache_creation) AS itpm,
					SUM(output_tokens)                 AS otpm,
					COUNT(*)                           AS rpm
				FROM requests
				WHERE end_time BETWEEN ? AND ?
				GROUP BY session_id, minute_epoch
			)
			SELECT
				'',
				MAX(itpm),
				MAX(otpm),
				MAX(rpm),
				MIN(minute_epoch),
				MAX(minute_epoch)
			FROM minute_windows`
	}

	rows, err := d.conn.Query(q, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TPMPeak
	for rows.Next() {
		var sessID sql.NullString
		var maxITPM, maxOTPM sql.NullFloat64
		var maxRPM sql.NullInt64
		var minEpoch, maxEpoch sql.NullInt64
		if err := rows.Scan(&sessID, &maxITPM, &maxOTPM, &maxRPM, &minEpoch, &maxEpoch); err != nil {
			return nil, err
		}
		// Skip the all-zero row that the "no data" all-time query produces.
		if !maxITPM.Valid && !maxOTPM.Valid && !maxRPM.Valid {
			continue
		}
		p := TPMPeak{
			SessionID: sessID.String,
		}
		if maxITPM.Valid {
			p.MaxITPM = maxITPM.Float64
		}
		if maxOTPM.Valid {
			p.MaxOTPM = maxOTPM.Float64
		}
		if maxRPM.Valid {
			p.MaxRPM = int(maxRPM.Int64)
		}
		if minEpoch.Valid {
			p.FirstSeen = time.Unix(minEpoch.Int64, 0).UTC()
		}
		if maxEpoch.Valid {
			p.LastSeen = time.Unix(maxEpoch.Int64, 0).UTC()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

Note: the TPMPeak timestamps (`MaxITPMTime` etc.) are left zero here because recovering the exact minute at which each individual peak occurred would require a more complex correlated query. `FirstSeen` and `LastSeen` give the range of minutes with data. The query command in Task 14 displays `FirstSeen`/`LastSeen` rather than per-metric peak timestamps.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestQueryTPM ./db`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add db/db.go db/db_test.go
git commit -m "db: add QueryTPMBuckets and QueryTPMPeak with group-by-session support"
```

---

## Task 14: Implement `--query tpm` command

**Files:**
- Modify: `query/query.go`
- Modify: `query/query_test.go`

- [ ] **Step 1: Write failing tests**

Append to `query/query_test.go`:

```go
func TestRunTPM_BucketedTable(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second),
		InputTokens: 1000, OutputTokens: 200, CacheCreation: 50, StatusCode: 200,
	})

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:   base.Add(-1 * time.Minute).Format("2006-01-02 15:04:05"),
		To:     base.Add(5 * time.Minute).Format("2006-01-02 15:04:05"),
		Bucket: 60,
		Writer: &buf,
	})
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "BUCKET_START") {
		t.Errorf("expected header BUCKET_START, got %q", out)
	}
	if !strings.Contains(out, "1050") { // 1000 + 50 cache creation
		t.Errorf("expected ITPM 1050 in output, got %q", out)
	}
}

func TestRunTPM_Peak(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second),
		InputTokens: 1000, OutputTokens: 200, StatusCode: 200,
	})

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:   base.Add(-1 * time.Minute).Format("2006-01-02 15:04:05"),
		To:     base.Add(5 * time.Minute).Format("2006-01-02 15:04:05"),
		Peak:   true,
		Writer: &buf,
	})
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "METRIC") || !strings.Contains(out, "ITPM") {
		t.Errorf("expected peak header, got %q", out)
	}
	if !strings.Contains(out, "1000") {
		t.Errorf("expected ITPM value 1000, got %q", out)
	}
}

func TestRunTPM_PeakGroupBySession(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: base, EndTime: base.Add(5 * time.Second), InputTokens: 1000, StatusCode: 200})
	d.InsertRecord(store.RequestRecord{SessionID: "s2", StartTime: base, EndTime: base.Add(5 * time.Second), InputTokens: 500, StatusCode: 200})

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:    base.Add(-1 * time.Minute).Format("2006-01-02 15:04:05"),
		To:      base.Add(5 * time.Minute).Format("2006-01-02 15:04:05"),
		Peak:    true,
		GroupBy: "session",
		Writer:  &buf,
	})
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "SESSION") {
		t.Errorf("expected SESSION header, got %q", out)
	}
	if !strings.Contains(out, "s1") || !strings.Contains(out, "s2") {
		t.Errorf("expected both sessions in output, got %q", out)
	}
}

func TestRunTPM_UnknownGroupBy(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		Peak:    true,
		GroupBy: "nonsense",
		Writer:  &buf,
	})
	if err == nil {
		t.Fatal("expected error for unknown group-by")
	}
}
```

Add imports to `query/query_test.go` if missing: `bytes`, `claude-proxy/db`, `claude-proxy/store`, `strings`, `time`.

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test -run TestRunTPM ./query`
Expected: compile error — `Opts.Bucket`, `Opts.Peak`, `Opts.GroupBy` don't exist yet.

- [ ] **Step 3: Extend `Opts` and add the `tpm` case**

Replace the `Opts` struct in `query/query.go` with:

```go
type Opts struct {
	From          string
	To            string
	SessionID     string
	TTFTThreshold int
	Limit         int
	Writer        io.Writer

	// TPM mode
	Bucket  int    // bucket size in seconds (default 300 = 5m)
	Peak    bool   // single-row peak mode
	GroupBy string // "" or "session"
}
```

Then update `RunQuery` to dispatch the new case:

```go
func RunQuery(d *db.DB, queryType string, opts Opts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	switch queryType {
	case "sessions":
		return runSessions(d, w, opts.Limit)
	case "requests":
		return runRequests(d, w, opts)
	case "throttle":
		return runThrottle(d, w, opts)
	case "summary":
		return runSummary(d, w, opts)
	case "tpm":
		return runTPM(d, w, opts)
	default:
		return fmt.Errorf("unknown query type: %s (valid: sessions, requests, throttle, summary, tpm)", queryType)
	}
}
```

And append `runTPM` to `query/query.go`:

```go
func runTPM(d *db.DB, w io.Writer, opts Opts) error {
	from, to, err := resolveTPMRange(opts)
	if err != nil {
		return err
	}

	if opts.Peak {
		return runTPMPeak(d, w, from, to, opts)
	}
	return runTPMBuckets(d, w, from, to, opts)
}

func resolveTPMRange(opts Opts) (time.Time, time.Time, error) {
	to := time.Now()
	from := to.Add(-1 * time.Hour)
	if opts.From != "" {
		t, err := time.Parse("2006-01-02 15:04:05", opts.From)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --from: %w", err)
		}
		from = t
	}
	if opts.To != "" {
		t, err := time.Parse("2006-01-02 15:04:05", opts.To)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --to: %w", err)
		}
		to = t
	}
	return from, to, nil
}

func runTPMBuckets(d *db.DB, w io.Writer, from, to time.Time, opts Opts) error {
	bucket := opts.Bucket
	if bucket <= 0 {
		bucket = 300 // 5 minutes
	}
	buckets, err := d.QueryTPMBuckets(from, to, bucket)
	if err != nil {
		return err
	}
	if opts.Limit > 0 && len(buckets) > opts.Limit {
		buckets = buckets[:opts.Limit]
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET_START\tMAX_ITPM\tMAX_OTPM\tMAX_RPM\tSAMPLES")
	for _, b := range buckets {
		fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%d\t%d\n",
			b.BucketStart.Local().Format("2006-01-02 15:04"),
			b.MaxITPM, b.MaxOTPM, b.MaxRPM, b.SampleMinutes)
	}
	return tw.Flush()
}

func runTPMPeak(d *db.DB, w io.Writer, from, to time.Time, opts Opts) error {
	peaks, err := d.QueryTPMPeak(from, to, opts.GroupBy)
	if err != nil {
		return err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(peaks) > limit {
		peaks = peaks[:limit]
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if opts.GroupBy == "session" {
		fmt.Fprintln(tw, "SESSION\tMAX_ITPM\tMAX_OTPM\tMAX_RPM\tFIRST_SEEN\tLAST_SEEN")
		for _, p := range peaks {
			fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%d\t%s\t%s\n",
				p.SessionID, p.MaxITPM, p.MaxOTPM, p.MaxRPM,
				p.FirstSeen.Local().Format("2006-01-02 15:04"),
				p.LastSeen.Local().Format("2006-01-02 15:04"))
		}
	} else {
		fmt.Fprintln(tw, "METRIC\tVALUE\tFIRST_SEEN\tLAST_SEEN")
		if len(peaks) == 0 {
			return tw.Flush()
		}
		p := peaks[0]
		fmt.Fprintf(tw, "ITPM\t%.0f\t%s\t%s\n", p.MaxITPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(tw, "OTPM\t%.0f\t%s\t%s\n", p.MaxOTPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(tw, "RPM\t%d\t%s\t%s\n", p.MaxRPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}
```

Add `"time"` to `query/query.go` imports if not already present.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test -run TestRunTPM ./query`
Expected: PASS.

- [ ] **Step 5: Run the full query test suite**

Run: `go test ./query`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add query/query.go query/query_test.go
git commit -m "query: add --query tpm with bucketed, peak, and group-by-session modes"
```

---

## Task 15: Rewrite TUI session and aggregate panes

**Files:**
- Modify: `tui/tui.go` (`renderSessionPane` ~line 124, `renderAggregatePane` ~line 239, `Update` ~line 44)

Replaces the old broken TPM display with the three-row ITPM/OTPM/RPM block from the spec. Calls `UpdateSessionPeaks` / `UpdateAggregatePeaks` on each tick. At this point the module-wide build will start working again (recall: Task 8 removed `CalculateTPM` methods that TUI was still calling).

- [ ] **Step 1: Verify the build is currently broken**

Run: `go build ./...`
Expected: FAIL with `store.(*Store).CalculateTPM undefined` and `store.(*Store).CalculateAggregateTPM undefined` in `tui/tui.go`.

This is expected state — Task 8 removed those methods. This task removes the callers.

- [ ] **Step 2: Update the tick handler in `Update` to compute and push peaks**

Replace the `tickMsg` case in the `Update` method of `tui/tui.go` (around line 74) with:

```go
	case tickMsg:
		m.refreshSessionList()
		// Compute current rolling metrics and update peaks for the visible session
		// and for the aggregate. Peaks are stored in the Store under store.mu.
		now := time.Now()
		itpm := m.store.RollingITPM(m.currentSession, now)
		otpm := m.store.RollingOTPM(m.currentSession, now)
		rpm := m.store.RollingRPM(m.currentSession, now)
		m.store.UpdateSessionPeaks(m.currentSession, itpm, otpm, rpm, now)

		aItpm := m.store.RollingAggregateITPM(now)
		aOtpm := m.store.RollingAggregateOTPM(now)
		aRpm := m.store.RollingAggregateRPM(now)
		m.store.UpdateAggregatePeaks(aItpm, aOtpm, aRpm, now)

		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})
```

- [ ] **Step 3: Replace `renderSessionPane` body**

Replace the `renderSessionPane` function (starting around line 124) with:

```go
func (m Model) renderSessionPane(sess *store.Session, inflight map[uint64]store.InFlightReq) string {
	var b strings.Builder
	uptime := time.Since(m.startTime).Truncate(time.Second)

	sessionLabel := m.currentSession
	if len(m.sessionList) > 1 {
		sessionLabel = fmt.Sprintf("%s [%d/%d]", m.currentSession, m.sessionIdx+1, len(m.sessionList))
	}

	activeTime := m.store.GetActiveTime(m.currentSession)
	activeStr := formatDuration(activeTime)
	b.WriteString(paneHeaderStyle.Render(fmt.Sprintf(" Session: %s ", sessionLabel)))
	b.WriteString(statLabelStyle.Render(fmt.Sprintf("  Duration: %s (active: %s)", uptime, activeStr)))
	b.WriteString("\n")

	now := time.Now()
	itpm := m.store.RollingITPM(m.currentSession, now)
	otpm := m.store.RollingOTPM(m.currentSession, now)
	rpm := m.store.RollingRPM(m.currentSession, now)
	peaks := m.store.GetSessionPeaks(m.currentSession)

	hasData := sess != nil && len(sess.Requests) > 0
	b.WriteString(fmt.Sprintf("  ITPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(itpm, hasData),
		formatRateFloat(peaks.MaxITPM, peaks.MaxITPM > 0),
		formatPeakTime(peaks.MaxITPMTime),
	))
	b.WriteString(fmt.Sprintf("  OTPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(otpm, hasData),
		formatRateFloat(peaks.MaxOTPM, peaks.MaxOTPM > 0),
		formatPeakTime(peaks.MaxOTPMTime),
	))
	b.WriteString(fmt.Sprintf("  RPM   |  current: %s     peak: %s  %s\n",
		formatRateInt(rpm, hasData),
		formatRateInt(peaks.MaxRPM, peaks.MaxRPM > 0),
		formatPeakTime(peaks.MaxRPMTime),
	))

	// Secondary totals line — cumulative counters for visibility
	var inputTok, outputTok, cacheRead, cacheCreate, reqCount int
	var totalLatency time.Duration
	if sess != nil {
		for _, r := range sess.Requests {
			inputTok += r.InputTokens
			outputTok += r.OutputTokens
			cacheRead += r.CacheRead
			cacheCreate += r.CacheCreation
			totalLatency += r.EndTime.Sub(r.StartTime)
		}
		reqCount = len(sess.Requests)
	}
	avgLatency := "--"
	if reqCount > 0 {
		avgLatency = fmt.Sprintf("%.1fs", (totalLatency / time.Duration(reqCount)).Seconds())
	}

	b.WriteString(fmt.Sprintf("  Totals: In %s  Out %s  Cache-R %s  Cache-W %s  Reqs %d  Avg %s\n",
		statValueStyle.Render(formatNum(inputTok)),
		statValueStyle.Render(formatNum(outputTok)),
		statValueStyle.Render(formatNum(cacheRead)),
		statValueStyle.Render(formatNum(cacheCreate)),
		reqCount,
		avgLatency,
	))

	return borderStyle.Width(maxInt(m.width-2, 60)).Render(b.String())
}

// formatRateFloat renders a float rate, returning "--" if there's no data yet.
func formatRateFloat(v float64, hasData bool) string {
	if !hasData {
		return "--"
	}
	return statValueStyle.Render(formatNum(int(v)))
}

// formatRateInt renders an integer rate, returning "--" if there's no data yet.
func formatRateInt(v int, hasData bool) string {
	if !hasData {
		return "--"
	}
	return statValueStyle.Render(formatNum(v))
}

// formatPeakTime returns "(HH:MM:SS)" for a non-zero time, or "" otherwise.
func formatPeakTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("(%s)", t.Format("15:04:05"))
}
```

- [ ] **Step 4: Replace `renderAggregatePane` body**

Replace `renderAggregatePane` (around line 239) with:

```go
func (m Model) renderAggregatePane() string {
	var b strings.Builder
	sessions := m.store.GetAllSessions()
	var totalReqs, totalTok int
	for _, s := range sessions {
		totalReqs += len(s.Requests)
		for _, r := range s.Requests {
			totalTok += r.InputTokens + r.OutputTokens + r.CacheRead + r.CacheCreation
		}
	}

	now := time.Now()
	aItpm := m.store.RollingAggregateITPM(now)
	aOtpm := m.store.RollingAggregateOTPM(now)
	aRpm := m.store.RollingAggregateRPM(now)
	peaks := m.store.GetAggregatePeaks()
	hasData := totalReqs > 0

	b.WriteString(fmt.Sprintf("  ITPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(aItpm, hasData),
		formatRateFloat(peaks.MaxITPM, peaks.MaxITPM > 0),
		formatPeakTime(peaks.MaxITPMTime),
	))
	b.WriteString(fmt.Sprintf("  OTPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(aOtpm, hasData),
		formatRateFloat(peaks.MaxOTPM, peaks.MaxOTPM > 0),
		formatPeakTime(peaks.MaxOTPMTime),
	))
	b.WriteString(fmt.Sprintf("  RPM   |  current: %s     peak: %s  %s\n",
		formatRateInt(aRpm, hasData),
		formatRateInt(peaks.MaxRPM, peaks.MaxRPM > 0),
		formatPeakTime(peaks.MaxRPMTime),
	))

	uptime := time.Since(m.startTime).Truncate(time.Second)
	b.WriteString(fmt.Sprintf("  Sessions: %s  Requests: %s  Total tok: %s  Uptime: %s\n",
		statValueStyle.Render(fmt.Sprintf("%d", len(sessions))),
		statValueStyle.Render(fmt.Sprintf("%d", totalReqs)),
		statValueStyle.Render(formatNum(totalTok)),
		statValueStyle.Render(uptime.String()),
	))

	return aggregateStyle.Width(maxInt(m.width-2, 60)).Render(
		paneHeaderStyle.Render(" Aggregate (All Sessions) ") + "\n" + b.String(),
	)
}
```

- [ ] **Step 5: Build the whole module to confirm it compiles**

Run: `go build ./...`
Expected: PASS — with the TUI no longer calling the removed methods.

- [ ] **Step 6: Run all tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 7: Run with -race**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add tui/tui.go
git commit -m "tui: replace single TPM display with ITPM/OTPM/RPM three-row block"
```

---

## Task 16: Wire new flags into `main.go`

**Files:**
- Modify: `main.go` (flag definitions and query dispatch around lines 24-70)

- [ ] **Step 1: Add the new flags and wire them into `query.Opts`**

In `main.go`, after the existing flag definitions (around line 33), add:

```go
	bucket := flag.Int("bucket", 0, "TPM query bucket size (e.g. 60, 300, 3600 seconds). 0 = default 5m")
	peak := flag.Bool("peak", false, "TPM query peak mode (single-row max)")
	groupBy := flag.String("group-by", "", "TPM query group-by: 'session' or empty")
```

Then update the `query.RunQuery` call (around lines 59-65) to pass the new fields:

```go
		err = query.RunQuery(d, *queryMode, query.Opts{
			From:          *from,
			To:            *to,
			TTFTThreshold: *ttftThreshold,
			Limit:         *limit,
			SessionID:     sessionFilter,
			Bucket:        *bucket,
			Peak:          *peak,
			GroupBy:       *groupBy,
		})
```

- [ ] **Step 2: Update the query mode flag description to include `tpm`**

Replace the `queryMode` flag line (around line 28):

```go
	queryMode := flag.String("query", "", "Query mode: sessions, requests, throttle, summary, tpm")
```

- [ ] **Step 3: Build and run a smoke test**

```bash
go build -o ccTPM .
./ccTPM --query tpm --db :memory: 2>&1 || true
```

Expected: the binary recognizes `tpm` as a query mode and prints an empty result (no records). Specifically, the error `unknown query type: tpm` should NOT appear. (A different error about `:memory:` is expected since we're running outside a proxy session.)

A cleaner smoke test: use an actual file.

```bash
./ccTPM --query tpm --db /tmp/cctpm-smoke.db --last 1h
```

Expected: empty bucketed output with headers only, or a table if there's data. No "unknown query type" error.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "main: wire --bucket, --peak, --group-by flags for tpm query mode"
```

---

## Task 17: Integration tests for end-to-end header capture and TPM measurement

**Files:**
- Modify: `integration_test.go`

- [ ] **Step 1: Read the existing integration_test.go to understand its patterns**

Run: `cat integration_test.go` and note how existing tests set up a mock upstream, proxy, and assert.

- [ ] **Step 2: Add the three new integration tests**

Append to `integration_test.go`:

```go
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
	buckets, err := d.QueryTPMBuckets(now.Add(-2*time.Minute), now.Add(1*time.Minute), 60)
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
```

Add any missing imports to `integration_test.go`: `claude-proxy/db`, `claude-proxy/proxy`, `claude-proxy/store`, `io`, `net/http`, `net/http/httptest`, `net/url`, `strings`, `testing`, `time`.

- [ ] **Step 2: Run integration tests to confirm they pass**

Run: `go test -run TestIntegration ./...`
Expected: PASS.

- [ ] **Step 3: Run the full test suite with -race**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add integration_test.go
git commit -m "integration: end-to-end tests for header capture and TPM measurement"
```

---

## Task 18: Final build + manual verification checklist

**Files:** none — verification only.

- [ ] **Step 1: Clean build**

```bash
go build -o ccTPM .
```
Expected: no errors.

- [ ] **Step 2: Full test suite**

```bash
go test ./...
```
Expected: all PASS.

- [ ] **Step 3: Race test**

```bash
go test -race ./...
```
Expected: all PASS, no race warnings.

- [ ] **Step 4: Vet**

```bash
go vet ./...
```
Expected: no issues.

- [ ] **Step 5: Smoke-test query mode against an empty DB**

```bash
./ccTPM --query tpm --db /tmp/cctpm-smoke.db --last 1h
```
Expected: a header row only (or nothing if DB is empty). No error.

```bash
./ccTPM --query tpm --peak --db /tmp/cctpm-smoke.db --last 1h
```
Expected: empty peak table. No error.

```bash
rm /tmp/cctpm-smoke.db*
```

- [ ] **Step 6: Manual live-proxy test (requires Claude Code installed)**

Start the proxy:
```bash
./ccTPM --port 8076 --session manual-test
```

In another terminal, point Claude Code at `http://localhost:8076` and run a normal coding task for 1–2 minutes.

Verify in the TUI:
- Session pane shows three rows: `ITPM`, `OTPM`, `RPM`.
- Each row has a `current: N` value that updates live.
- After a few requests, `peak: N (HH:MM:SS)` appears.
- Bottom `Totals:` line shows accumulated tokens.
- Aggregate pane shows similar three-row structure.

Stop the proxy (`q` or `Ctrl-C`).

- [ ] **Step 7: Verify DB header capture for a real OAuth session**

```bash
sqlite3 ~/.claude-proxy/data.db "SELECT unified_5h_util, unified_5h_status, unified_repr_claim FROM requests ORDER BY start_time DESC LIMIT 5;"
```
Expected on an OAuth session: non-NULL values for `unified_5h_util` (a small decimal like `0.0184`), `unified_5h_status` (`"allowed"`), and `unified_repr_claim` (e.g., `"five_hour"`).

If all values are NULL, the headers were not present in the response — that indicates either (a) you're running against API-key traffic (check `itokens_limit` instead, which should be populated), or (b) the OAuth traffic is going to a different host than `api.anthropic.com`.

- [ ] **Step 8: Verify historical query mode**

```bash
./ccTPM --query tpm --last 1h --bucket 300
```
Expected: a table with at least one bucket showing `MAX_ITPM`, `MAX_OTPM`, `MAX_RPM` values reflecting the session you just ran.

```bash
./ccTPM --query tpm --peak --last 24h
```
Expected: a three-row table with `METRIC / VALUE / FIRST_SEEN / LAST_SEEN` for ITPM, OTPM, RPM.

```bash
./ccTPM --query tpm --peak --group-by session --last 7d
```
Expected: one row per distinct session seen in the last 7 days.

- [ ] **Step 9: Final commit marking completion**

```bash
git add -A
git status   # should show nothing unstaged — any artifacts removed
git commit --allow-empty -m "feat(cctpm): complete ITPM/OTPM/RPM throughput measurement feature

Implements the spec in docs/superpowers/specs/2026-04-05-itpm-otpm-rpm-design.md:
- Three rolling-60s metrics matching Anthropic's official ITPM/OTPM/RPM
  definitions, replacing the broken active-time TPM formula
- SessionPeaks and aggregate peaks tracked at TUI tick cadence
- Fixed proxy rate-limit header capture for anthropic-ratelimit-* and
  OAuth anthropic-ratelimit-unified-* header sets
- Additive SQL migration with 17 new nullable columns
- New --query tpm command with bucketed, peak, and group-by-session modes
- TUI session and aggregate panes redesigned with three-row metric blocks"
```

---

## Success Criteria

All of the following must hold for the feature to be considered done:

1. `go test ./...` passes.
2. `go test -race ./...` passes.
3. `go vet ./...` is clean.
4. `go build -o ccTPM .` succeeds.
5. Manual TUI test (Step 6) shows ITPM/OTPM/RPM live-updating during a real Claude Code OAuth session.
6. Peak values persist with correct timestamps.
7. `./ccTPM --query tpm --peak` reports non-zero peaks after a session.
8. `sqlite3` query (Step 7) shows populated `unified_5h_util` / `unified_status` / `unified_repr_claim` columns for an OAuth session (proving the header-capture fix works against real traffic).
9. The old single `TPM: N` number is no longer visible anywhere in the TUI.
10. `store.CalculateTPM` and `store.CalculateAggregateTPM` no longer exist in the codebase.

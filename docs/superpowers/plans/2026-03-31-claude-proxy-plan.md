# Claude Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go reverse proxy that sits between Claude Code and the Anthropic API, tracking token usage per request and calculating tokens-per-minute (TPM) with a live TUI dashboard and SQLite persistence for throttling analysis.

**Architecture:** Single-binary Go app. `httputil.ReverseProxy` with `Rewrite` forwards requests to `api.anthropic.com`. `ModifyResponse` wraps `resp.Body` with a tee-reader that sends byte copies to a parser goroutine via buffered channel. Parser extracts token usage from JSON or SSE responses. Records go to an in-memory store (for TUI) and a SQLite DB (for historical queries). Bubbletea TUI renders live stats on a 1-second tick.

**Tech Stack:** Go 1.22+, `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `modernc.org/sqlite`, stdlib (`net/http/httputil`, `database/sql`, `encoding/json`, `sync`)

**Spec:** `docs/superpowers/specs/2026-03-31-claude-proxy-design.md`

---

## File Map

| File | Responsibility |
|------|---------------|
| `go.mod`, `go.sum` | Module definition, dependencies |
| `store/store.go` | Shared types (`RequestRecord`, `Session`, `InFlightReq`), in-memory `Store` with mutex, TPM calculation via merged intervals |
| `store/store_test.go` | Store unit tests |
| `parser/parser.go` | `Result` type, `ParseJSON` for non-streaming, `ParseSSE` for SSE streams |
| `parser/parser_test.go` | Parser unit tests |
| `proxy/proxy.go` | `teeReadCloser` (TTFT + buffered channel), `NewProxy` factory, `modifyResponse` hook, session ID extraction, header extraction |
| `proxy/proxy_test.go` | Proxy unit tests |
| `db/db.go` | SQLite open (WAL mode), schema creation, `InsertRecord`, query functions for sessions/requests/throttle/summary |
| `db/db_test.go` | DB unit tests |
| `tui/tui.go` | Bubbletea `Model` (Init/Update/View), tick subscription, session cycling, request log scrolling |
| `tui/styles.go` | Lipgloss style definitions for all TUI panes |
| `query/query.go` | CLI `--query` mode: format and print results from DB queries |
| `query/query_test.go` | Query formatting tests |
| `main.go` | CLI flag parsing, wiring (store + db + proxy + tui), startup/shutdown orchestration, query mode routing |

---

### Task 1: Project Scaffolding

**Files:**
- Create: `go.mod`
- Create directory structure: `store/`, `parser/`, `proxy/`, `db/`, `tui/`, `query/`

- [ ] **Step 1: Initialize Go module**

```bash
cd /Users/apple-dev/Documents/ClaudePlugin
go mod init claude-proxy
```

- [ ] **Step 2: Install dependencies**

```bash
go get charm.land/bubbletea/v2@latest
go get charm.land/lipgloss/v2@latest
go get modernc.org/sqlite@latest
```

- [ ] **Step 3: Create directory structure**

```bash
mkdir -p store parser proxy db tui query
```

- [ ] **Step 4: Create placeholder main.go so the module compiles**

```go
// main.go
package main

func main() {}
```

- [ ] **Step 5: Verify build**

Run: `go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum main.go store/ parser/ proxy/ db/ tui/ query/
git commit -m "feat: scaffold project structure and dependencies"
```

---

### Task 2: Store Types and Basic Operations

**Files:**
- Create: `store/store.go`
- Create: `store/store_test.go`

- [ ] **Step 1: Write failing tests for store types and basic operations**

```go
// store/store_test.go
package store

import (
	"testing"
	"time"
)

func TestNewStore(t *testing.T) {
	s := NewStore()
	if s == nil {
		t.Fatal("NewStore returned nil")
	}
	sessions := s.GetAllSessions()
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestAddRecord(t *testing.T) {
	s := NewStore()
	rec := RequestRecord{
		StartTime:   time.Now().Add(-2 * time.Second),
		EndTime:     time.Now(),
		Model:       "claude-sonnet-4-20250514",
		InputTokens: 1000,
		OutputTokens: 500,
		Endpoint:    "/v1/messages",
		StatusCode:  200,
		TTFT:        100 * time.Millisecond,
		SessionID:   "test-session",
	}
	s.AddRecord(rec)

	sess := s.GetSession("test-session")
	if sess == nil {
		t.Fatal("session not found")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(sess.Requests))
	}
	if sess.Requests[0].InputTokens != 1000 {
		t.Fatalf("expected 1000 input tokens, got %d", sess.Requests[0].InputTokens)
	}
}

func TestAddRecordCreatesSession(t *testing.T) {
	s := NewStore()
	rec := RequestRecord{
		StartTime: time.Now(),
		EndTime:   time.Now(),
		SessionID: "new-session",
		StatusCode: 200,
	}
	s.AddRecord(rec)

	sessions := s.GetAllSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "new-session" {
		t.Fatalf("expected session ID 'new-session', got '%s'", sessions[0].ID)
	}
}

func TestInflight(t *testing.T) {
	s := NewStore()
	id := s.AddInFlight("sess1", "/v1/messages")
	inflight := s.GetInFlight()
	if len(inflight) != 1 {
		t.Fatalf("expected 1 inflight, got %d", len(inflight))
	}
	if inflight[id].SessionID != "sess1" {
		t.Fatalf("expected session 'sess1', got '%s'", inflight[id].SessionID)
	}

	s.RemoveInFlight(id)
	inflight = s.GetInFlight()
	if len(inflight) != 0 {
		t.Fatalf("expected 0 inflight after remove, got %d", len(inflight))
	}
}

func TestGetAllSessionsSortedByLastSeen(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(RequestRecord{StartTime: now.Add(-10 * time.Minute), EndTime: now.Add(-10 * time.Minute), SessionID: "old", StatusCode: 200})
	s.AddRecord(RequestRecord{StartTime: now, EndTime: now, SessionID: "new", StatusCode: 200})

	sessions := s.GetAllSessions()
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].ID != "new" {
		t.Fatalf("expected most recent session first, got '%s'", sessions[0].ID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./store/ -v`
Expected: Compilation errors — types and functions not defined.

- [ ] **Step 3: Implement store types and basic operations**

```go
// store/store.go
package store

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

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
}

type Session struct {
	ID        string
	StartTime time.Time
	LastSeen  time.Time
	Requests  []RequestRecord
}

type InFlightReq struct {
	SessionID string
	StartTime time.Time
	Endpoint  string
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	inflight map[uint64]InFlightReq
	nextID   atomic.Uint64
}

func NewStore() *Store {
	return &Store{
		sessions: make(map[string]*Session),
		inflight: make(map[uint64]InFlightReq),
	}
}

func (s *Store) AddRecord(rec RequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[rec.SessionID]
	if !ok {
		sess = &Session{
			ID:        rec.SessionID,
			StartTime: rec.StartTime,
		}
		s.sessions[rec.SessionID] = sess
	}
	sess.LastSeen = rec.EndTime
	sess.Requests = append(sess.Requests, rec)
}

func (s *Store) GetSession(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	cp := *sess
	cp.Requests = make([]RequestRecord, len(sess.Requests))
	copy(cp.Requests, sess.Requests)
	return &cp
}

func (s *Store) GetAllSessions() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		cp := *sess
		cp.Requests = make([]RequestRecord, len(sess.Requests))
		copy(cp.Requests, sess.Requests)
		sessions = append(sessions, cp)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastSeen.After(sessions[j].LastSeen)
	})
	return sessions
}

func (s *Store) AddInFlight(sessionID, endpoint string) uint64 {
	id := s.nextID.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[id] = InFlightReq{
		SessionID: sessionID,
		StartTime: time.Now(),
		Endpoint:  endpoint,
	}
	return id
}

func (s *Store) RemoveInFlight(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

func (s *Store) GetInFlight() map[uint64]InFlightReq {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[uint64]InFlightReq, len(s.inflight))
	for k, v := range s.inflight {
		cp[k] = v
	}
	return cp
}

func (s *Store) InFlightCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.inflight)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./store/ -v`
Expected: All 5 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add store/
git commit -m "feat: add store types and basic operations"
```

---

### Task 3: TPM Calculation (Merged Intervals)

**Files:**
- Modify: `store/store.go`
- Modify: `store/store_test.go`

- [ ] **Step 1: Write failing tests for merged intervals and TPM**

```go
// Append to store/store_test.go

func TestMergeIntervalsNoOverlap(t *testing.T) {
	intervals := []interval{
		{start: time.Unix(0, 0), end: time.Unix(10, 0)},
		{start: time.Unix(20, 0), end: time.Unix(30, 0)},
	}
	merged := mergeIntervals(intervals)
	if len(merged) != 2 {
		t.Fatalf("expected 2 intervals, got %d", len(merged))
	}
	totalDur := sumDurations(merged)
	if totalDur != 20*time.Second {
		t.Fatalf("expected 20s, got %v", totalDur)
	}
}

func TestMergeIntervalsOverlapping(t *testing.T) {
	intervals := []interval{
		{start: time.Unix(0, 0), end: time.Unix(30, 0)},
		{start: time.Unix(10, 0), end: time.Unix(30, 0)},
	}
	merged := mergeIntervals(intervals)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged interval, got %d", len(merged))
	}
	totalDur := sumDurations(merged)
	if totalDur != 30*time.Second {
		t.Fatalf("expected 30s, got %v", totalDur)
	}
}

func TestMergeIntervalsAdjacent(t *testing.T) {
	intervals := []interval{
		{start: time.Unix(0, 0), end: time.Unix(10, 0)},
		{start: time.Unix(10, 0), end: time.Unix(20, 0)},
	}
	merged := mergeIntervals(intervals)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged interval, got %d", len(merged))
	}
}

func TestMergeIntervalsEmpty(t *testing.T) {
	merged := mergeIntervals(nil)
	if len(merged) != 0 {
		t.Fatalf("expected 0 intervals, got %d", len(merged))
	}
}

func TestCalculateTPM(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// 2 sequential requests, each 30s, 5000 tokens each = 10000 tokens in 60s = 10000 TPM
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-60 * time.Second), EndTime: now.Add(-30 * time.Second),
		InputTokens: 3000, OutputTokens: 2000, SessionID: "s1", StatusCode: 200,
	})
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		InputTokens: 3000, OutputTokens: 2000, SessionID: "s1", StatusCode: 200,
	})

	tpm := s.CalculateTPM("s1")
	if tpm < 9900 || tpm > 10100 {
		t.Fatalf("expected ~10000 TPM, got %.0f", tpm)
	}
}

func TestCalculateTPMConcurrent(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// 2 concurrent requests, same 30s window, 5000 tokens each = 10000 tokens in 30s = 20000 TPM
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		InputTokens: 3000, OutputTokens: 2000, SessionID: "s1", StatusCode: 200,
	})
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		InputTokens: 3000, OutputTokens: 2000, SessionID: "s1", StatusCode: 200,
	})

	tpm := s.CalculateTPM("s1")
	if tpm < 19900 || tpm > 20100 {
		t.Fatalf("expected ~20000 TPM, got %.0f", tpm)
	}
}

func TestCalculateTPMZeroActiveTime(t *testing.T) {
	s := NewStore()
	tpm := s.CalculateTPM("nonexistent")
	if tpm != 0 {
		t.Fatalf("expected 0 TPM for nonexistent session, got %.0f", tpm)
	}
}

func TestCalculateAggregateTPM(t *testing.T) {
	s := NewStore()
	now := time.Now()

	s.AddRecord(RequestRecord{
		StartTime: now.Add(-60 * time.Second), EndTime: now.Add(-30 * time.Second),
		InputTokens: 5000, OutputTokens: 0, SessionID: "s1", StatusCode: 200,
	})
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		InputTokens: 5000, OutputTokens: 0, SessionID: "s2", StatusCode: 200,
	})

	tpm := s.CalculateAggregateTPM()
	// 10000 tokens, 60s active = 10000 TPM
	if tpm < 9900 || tpm > 10100 {
		t.Fatalf("expected ~10000 TPM, got %.0f", tpm)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./store/ -v -run "Merge|TPM"`
Expected: Compilation errors — `interval`, `mergeIntervals`, `sumDurations`, `CalculateTPM`, `CalculateAggregateTPM` not defined.

- [ ] **Step 3: Implement merged intervals and TPM calculation**

Append to `store/store.go`:

```go
type interval struct {
	start time.Time
	end   time.Time
}

func mergeIntervals(intervals []interval) []interval {
	if len(intervals) == 0 {
		return nil
	}
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].start.Before(intervals[j].start)
	})
	merged := []interval{intervals[0]}
	for _, iv := range intervals[1:] {
		last := &merged[len(merged)-1]
		if !iv.start.After(last.end) {
			if iv.end.After(last.end) {
				last.end = iv.end
			}
		} else {
			merged = append(merged, iv)
		}
	}
	return merged
}

func sumDurations(intervals []interval) time.Duration {
	var total time.Duration
	for _, iv := range intervals {
		total += iv.end.Sub(iv.start)
	}
	return total
}

func (s *Store) collectIntervals(sessionID string) []interval {
	var intervals []interval
	if sessionID != "" {
		sess, ok := s.sessions[sessionID]
		if !ok {
			return nil
		}
		for _, r := range sess.Requests {
			intervals = append(intervals, interval{start: r.StartTime, end: r.EndTime})
		}
	} else {
		for _, sess := range s.sessions {
			for _, r := range sess.Requests {
				intervals = append(intervals, interval{start: r.StartTime, end: r.EndTime})
			}
		}
	}
	now := time.Now()
	for _, inf := range s.inflight {
		if sessionID == "" || inf.SessionID == sessionID {
			intervals = append(intervals, interval{start: inf.StartTime, end: now})
		}
	}
	return intervals
}

func (s *Store) calculateTPMFromIntervals(intervals []interval) float64 {
	merged := mergeIntervals(intervals)
	activeTime := sumDurations(merged)
	if activeTime == 0 {
		return 0
	}
	var totalTokens int
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Caller must NOT hold lock — but we need tokens. Refactor below.
	return float64(totalTokens) / activeTime.Minutes()
}

// CalculateTPM returns the TPM for a specific session.
// Returns 0 if session doesn't exist or has no active time.
func (s *Store) CalculateTPM(sessionID string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}

	intervals := s.collectIntervals(sessionID)
	merged := mergeIntervals(intervals)
	activeTime := sumDurations(merged)
	if activeTime == 0 {
		return 0
	}

	var totalTokens int
	for _, r := range sess.Requests {
		totalTokens += r.InputTokens + r.OutputTokens + r.CacheCreation + r.CacheRead
	}
	return float64(totalTokens) / activeTime.Minutes()
}

// CalculateAggregateTPM returns the TPM across all sessions.
func (s *Store) CalculateAggregateTPM() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	intervals := s.collectIntervals("")
	merged := mergeIntervals(intervals)
	activeTime := sumDurations(merged)
	if activeTime == 0 {
		return 0
	}

	var totalTokens int
	for _, sess := range s.sessions {
		for _, r := range sess.Requests {
			totalTokens += r.InputTokens + r.OutputTokens + r.CacheCreation + r.CacheRead
		}
	}
	return float64(totalTokens) / activeTime.Minutes()
}
```

Remove the unused `calculateTPMFromIntervals` method (it was a false start in the code above — the actual logic is in `CalculateTPM` and `CalculateAggregateTPM`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./store/ -v`
Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add store/
git commit -m "feat: add TPM calculation with merged time intervals"
```

---

### Task 4: SQLite DB Layer

**Files:**
- Create: `db/db.go`
- Create: `db/db_test.go`

- [ ] **Step 1: Write failing tests for DB open, insert, and query**

```go
// db/db_test.go
package db

import (
	"claude-proxy/store"
	"testing"
	"time"
)

func TestOpenAndClose(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer d.Close()
}

func TestInsertAndQueryRequests(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	now := time.Now()
	rec := store.RequestRecord{
		StartTime:             now.Add(-5 * time.Second),
		EndTime:               now,
		Model:                 "claude-sonnet-4-20250514",
		InputTokens:           1000,
		OutputTokens:          500,
		CacheCreation:         0,
		CacheRead:             200,
		Endpoint:              "/v1/messages",
		StatusCode:            200,
		TTFT:                  150 * time.Millisecond,
		RetryAfter:            "",
		RateLimitReqRemaining: 50,
		RateLimitTokRemaining: 100000,
		HasError:              false,
		SessionID:             "sess1",
	}

	if err := d.InsertRecord(rec); err != nil {
		t.Fatalf("InsertRecord failed: %v", err)
	}

	rows, err := d.QueryRequests("", "", "")
	if err != nil {
		t.Fatalf("QueryRequests failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", rows[0].Model)
	}
}

func TestQuerySessions(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	now := time.Now()
	for i := 0; i < 3; i++ {
		d.InsertRecord(store.RequestRecord{
			StartTime: now.Add(time.Duration(-i) * time.Minute),
			EndTime:   now.Add(time.Duration(-i)*time.Minute + 5*time.Second),
			InputTokens: 1000, OutputTokens: 500,
			SessionID: "sess1", StatusCode: 200,
		})
	}
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(2 * time.Second),
		InputTokens: 2000, OutputTokens: 1000,
		SessionID: "sess2", StatusCode: 200,
	})

	sessions, err := d.QuerySessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestQueryThrottle(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	now := time.Now()
	// Normal request
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(time.Second),
		StatusCode: 200, SessionID: "s1", TTFT: 100 * time.Millisecond,
	})
	// 429 request
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(time.Second),
		StatusCode: 429, SessionID: "s1", RetryAfter: "30",
	})
	// SSE error
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(time.Second),
		StatusCode: 200, SessionID: "s1", HasError: true,
	})
	// High TTFT
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(time.Second),
		StatusCode: 200, SessionID: "s1", TTFT: 6 * time.Second,
	})

	rows, err := d.QueryThrottle(5000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Should match: 429, SSE error, high TTFT (not the normal one)
	if len(rows) != 3 {
		t.Fatalf("expected 3 throttle events, got %d", len(rows))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./db/ -v`
Expected: Compilation errors.

- [ ] **Step 3: Implement DB layer**

```go
// db/db.go
package db

import (
	"claude-proxy/store"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type SessionSummary struct {
	ID           string
	RequestCount int
	TotalTokens  int
	FirstSeen    time.Time
	LastSeen     time.Time
}

type DB struct {
	conn *sql.DB
}

func Open(dsn string) (*DB, error) {
	uri := dsn
	if dsn != ":memory:" {
		uri = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate", dsn)
	}
	conn, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	d := &DB{conn: conn}
	if err := d.createSchema(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

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
	return err
}

func (d *DB) InsertRecord(rec store.RequestRecord) error {
	_, err := d.conn.Exec(`
		INSERT INTO requests (
			session_id, start_time, end_time, model, endpoint, status_code,
			input_tokens, output_tokens, cache_creation, cache_read,
			ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SessionID, rec.StartTime.UTC(), rec.EndTime.UTC(), rec.Model, rec.Endpoint, rec.StatusCode,
		rec.InputTokens, rec.OutputTokens, rec.CacheCreation, rec.CacheRead,
		rec.TTFT.Milliseconds(), rec.RetryAfter, rec.RateLimitReqRemaining, rec.RateLimitTokRemaining,
		boolToInt(rec.HasError),
	)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *DB) QueryRequests(from, to, sessionID string) ([]store.RequestRecord, error) {
	q := "SELECT session_id, start_time, end_time, model, endpoint, status_code, input_tokens, output_tokens, cache_creation, cache_read, ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error FROM requests WHERE 1=1"
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

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (d *DB) QuerySessions() ([]SessionSummary, error) {
	rows, err := d.conn.Query(`
		SELECT session_id,
			COUNT(*) as request_count,
			SUM(input_tokens + output_tokens + cache_creation + cache_read) as total_tokens,
			MIN(start_time) as first_seen,
			MAX(end_time) as last_seen
		FROM requests
		GROUP BY session_id
		ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(&s.ID, &s.RequestCount, &s.TotalTokens, &s.FirstSeen, &s.LastSeen); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (d *DB) QueryThrottle(ttftThresholdMs int, from, to, sessionID string) ([]store.RequestRecord, error) {
	q := `SELECT session_id, start_time, end_time, model, endpoint, status_code,
		input_tokens, output_tokens, cache_creation, cache_read,
		ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error
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

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (d *DB) QuerySummary(from, to string) (map[string]any, error) {
	q := `SELECT
		COUNT(*) as total_requests,
		COALESCE(SUM(input_tokens + output_tokens + cache_creation + cache_read), 0) as total_tokens,
		COUNT(DISTINCT session_id) as session_count,
		COALESCE(AVG(ttft_ms), 0) as avg_ttft_ms,
		SUM(CASE WHEN status_code IN (429, 529) OR has_error = 1 THEN 1 ELSE 0 END) as throttle_events
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

	var totalReqs, totalTokens, sessionCount, throttleEvents int
	var avgTTFT float64
	err := d.conn.QueryRow(q, args...).Scan(&totalReqs, &totalTokens, &sessionCount, &avgTTFT, &throttleEvents)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"total_requests": totalReqs,
		"total_tokens":   totalTokens,
		"session_count":  sessionCount,
		"avg_ttft_ms":    avgTTFT,
		"throttle_events": throttleEvents,
	}, nil
}

func scanRecords(rows *sql.Rows) ([]store.RequestRecord, error) {
	var records []store.RequestRecord
	for rows.Next() {
		var r store.RequestRecord
		var ttftMs int64
		var hasErr int
		if err := rows.Scan(
			&r.SessionID, &r.StartTime, &r.EndTime, &r.Model, &r.Endpoint, &r.StatusCode,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreation, &r.CacheRead,
			&ttftMs, &r.RetryAfter, &r.RateLimitReqRemaining, &r.RateLimitTokRemaining, &hasErr,
		); err != nil {
			return nil, err
		}
		r.TTFT = time.Duration(ttftMs) * time.Millisecond
		r.HasError = hasErr != 0
		records = append(records, r)
	}
	return records, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./db/ -v`
Expected: All 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add db/
git commit -m "feat: add SQLite persistence layer with schema and query functions"
```

---

### Task 5: Response Parsers (JSON + SSE)

**Files:**
- Create: `parser/parser.go`
- Create: `parser/parser_test.go`

- [ ] **Step 1: Write failing tests for JSON and SSE parsing**

```go
// parser/parser_test.go
package parser

import (
	"strings"
	"testing"
)

func TestParseJSON(t *testing.T) {
	body := `{
		"id": "msg_123",
		"type": "message",
		"model": "claude-sonnet-4-20250514",
		"usage": {
			"input_tokens": 1234,
			"output_tokens": 567,
			"cache_creation_input_tokens": 100,
			"cache_read_input_tokens": 800
		}
	}`
	result, err := ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON failed: %v", err)
	}
	if result.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", result.Model)
	}
	if result.InputTokens != 1234 {
		t.Fatalf("expected 1234 input tokens, got %d", result.InputTokens)
	}
	if result.OutputTokens != 567 {
		t.Fatalf("expected 567 output tokens, got %d", result.OutputTokens)
	}
	if result.CacheCreation != 100 {
		t.Fatalf("expected 100 cache creation, got %d", result.CacheCreation)
	}
	if result.CacheRead != 800 {
		t.Fatalf("expected 800 cache read, got %d", result.CacheRead)
	}
}

func TestParseJSONIgnoresUnknownFields(t *testing.T) {
	body := `{"model": "x", "usage": {"input_tokens": 1, "output_tokens": 2, "server_tool_use": {"web_search_requests": 1}}, "unknown_field": true}`
	result, err := ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON failed on unknown fields: %v", err)
	}
	if result.InputTokens != 1 {
		t.Fatalf("expected 1, got %d", result.InputTokens)
	}
}

func TestParseSSEBasic(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":1234,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":800}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":567}}

event: message_stop
data: {"type":"message_stop"}

`
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseSSE failed: %v", err)
	}
	if result.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", result.Model)
	}
	if result.InputTokens != 1234 {
		t.Fatalf("expected 1234 input, got %d", result.InputTokens)
	}
	if result.OutputTokens != 567 {
		t.Fatalf("expected 567 output, got %d", result.OutputTokens)
	}
	if result.CacheRead != 800 {
		t.Fatalf("expected 800 cache read, got %d", result.CacheRead)
	}
}

func TestParseSSEMessageDeltaOverridesInputTokens(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":100,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5000,"output_tokens":200}}

`
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 5000 {
		t.Fatalf("expected message_delta to override input_tokens to 5000, got %d", result.InputTokens)
	}
	if result.OutputTokens != 200 {
		t.Fatalf("expected 200 output, got %d", result.OutputTokens)
	}
}

func TestParseSSEMissingUsageInDelta(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":100,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}

`
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	// Should retain message_start values
	if result.InputTokens != 100 {
		t.Fatalf("expected 100 input, got %d", result.InputTokens)
	}
	if result.OutputTokens != 1 {
		t.Fatalf("expected 1 output, got %d", result.OutputTokens)
	}
}

func TestParseSSEErrorEvent(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":100,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasError {
		t.Fatal("expected HasError=true for error event")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./parser/ -v`
Expected: Compilation errors.

- [ ] **Step 3: Implement parsers**

```go
// parser/parser.go
package parser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type Result struct {
	Model         string
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
	HasError      bool
}

type jsonResponse struct {
	Model string    `json:"model"`
	Usage jsonUsage `json:"usage"`
}

type jsonUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead    int `json:"cache_read_input_tokens"`
}

func ParseJSON(body []byte) (Result, error) {
	var resp jsonResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Result{}, fmt.Errorf("parse json response: %w", err)
	}
	return Result{
		Model:         resp.Model,
		InputTokens:   resp.Usage.InputTokens,
		OutputTokens:  resp.Usage.OutputTokens,
		CacheCreation: resp.Usage.CacheCreation,
		CacheRead:     resp.Usage.CacheRead,
	}, nil
}

type sseMessageStart struct {
	Type    string `json:"type"`
	Message struct {
		Model string    `json:"model"`
		Usage jsonUsage `json:"usage"`
	} `json:"message"`
}

type sseMessageDelta struct {
	Type  string     `json:"type"`
	Usage *jsonUsage `json:"usage,omitempty"`
}

func ParseSSE(r io.Reader) (Result, error) {
	var result Result
	scanner := bufio.NewScanner(r)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			switch currentEvent {
			case "message_start":
				var msg sseMessageStart
				if err := json.Unmarshal([]byte(data), &msg); err == nil {
					result.Model = msg.Message.Model
					result.InputTokens = msg.Message.Usage.InputTokens
					result.OutputTokens = msg.Message.Usage.OutputTokens
					result.CacheCreation = msg.Message.Usage.CacheCreation
					result.CacheRead = msg.Message.Usage.CacheRead
				}

			case "message_delta":
				var msg sseMessageDelta
				if err := json.Unmarshal([]byte(data), &msg); err == nil && msg.Usage != nil {
					if msg.Usage.InputTokens != 0 {
						result.InputTokens = msg.Usage.InputTokens
					}
					if msg.Usage.OutputTokens != 0 {
						result.OutputTokens = msg.Usage.OutputTokens
					}
					if msg.Usage.CacheCreation != 0 {
						result.CacheCreation = msg.Usage.CacheCreation
					}
					if msg.Usage.CacheRead != 0 {
						result.CacheRead = msg.Usage.CacheRead
					}
				}

			case "error":
				result.HasError = true
				return result, nil
			}
			currentEvent = ""
		}
	}
	return result, scanner.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./parser/ -v`
Expected: All 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add parser/
git commit -m "feat: add JSON and SSE response parsers with message_delta override logic"
```

---

### Task 6: Tee-Reader and Proxy

**Files:**
- Create: `proxy/proxy.go`
- Create: `proxy/proxy_test.go`

- [ ] **Step 1: Write failing tests for teeReadCloser and session ID extraction**

```go
// proxy/proxy_test.go
package proxy

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

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

	time.Sleep(10 * time.Millisecond) // Simulate delay
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

	// Channel should be closed — reading should not block
	select {
	case _, ok := <-ch:
		if ok {
			// Drain remaining
			for range ch {}
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./proxy/ -v`
Expected: Compilation errors.

- [ ] **Step 3: Implement teeReadCloser and proxy setup**

```go
// proxy/proxy.go
package proxy

import (
	"claude-proxy/parser"
	"claude-proxy/store"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
			// Channel full — drop chunk (parser will have partial data).
			// This should be rare with a 64-slot buffer.
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
	requestStart := time.Now() // Approximate — ideally captured in Rewrite
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
		endTime := time.Now()

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
		endTime = time.Now()

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
		}
	}()

	return nil
}

// channelReader adapts a chan []byte to io.Reader for the SSE parser.
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./proxy/ -v`
Expected: All 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add proxy/
git commit -m "feat: add tee-reader proxy with TTFT measurement and session ID extraction"
```

---

### Task 7: TUI Styles

**Files:**
- Create: `tui/styles.go`

- [ ] **Step 1: Implement TUI styles**

```go
// tui/styles.go
package tui

import (
	"charm.land/lipgloss/v2"
)

var (
	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("170"))

	paneHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("39"))

	statLabelStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("245"))

	statValueStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("255"))

	requestRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	errorRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("196"))

	inflightRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("214"))

	borderStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63"))

	aggregateStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240"))

	tpmStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("46"))
)
```

- [ ] **Step 2: Verify build**

Run: `go build ./tui/`
Expected: Clean build.

- [ ] **Step 3: Commit**

```bash
git add tui/styles.go
git commit -m "feat: add TUI lipgloss style definitions"
```

---

### Task 8: TUI Model

**Files:**
- Create: `tui/tui.go`

- [ ] **Step 1: Implement bubbletea model**

```go
// tui/tui.go
package tui

import (
	"claude-proxy/store"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type tickMsg time.Time

type Model struct {
	store          *store.Store
	currentSession string
	sessionList    []string
	sessionIdx     int
	scrollOffset   int
	maxLogRows     int
	width          int
	height         int
	quitting       bool
	startTime      time.Time
	shutdownMsg    string
}

func NewModel(s *store.Store, initialSession string) Model {
	return Model{
		store:          s,
		currentSession: initialSession,
		sessionList:    []string{initialSession},
		maxLogRows:     10,
		startTime:      time.Now(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.maxLogRows = max(m.height-12, 3)

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			inflight := m.store.InFlightCount()
			if inflight > 0 {
				m.shutdownMsg = fmt.Sprintf("waiting for %d in-flight request(s)...", inflight)
			}
			m.quitting = true
			return m, tea.Quit
		case "tab":
			if len(m.sessionList) > 1 {
				m.sessionIdx = (m.sessionIdx + 1) % len(m.sessionList)
				m.currentSession = m.sessionList[m.sessionIdx]
				m.scrollOffset = 0
			}
		case "j", "down":
			m.scrollOffset++
		case "k", "up":
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		}

	case tickMsg:
		m.refreshSessionList()
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})
	}
	return m, nil
}

func (m *Model) refreshSessionList() {
	sessions := m.store.GetAllSessions()
	m.sessionList = make([]string, len(sessions))
	for i, s := range sessions {
		m.sessionList[i] = s.ID
	}
	if len(m.sessionList) == 0 {
		m.sessionList = []string{m.currentSession}
	}
	// Keep sessionIdx valid
	found := false
	for i, id := range m.sessionList {
		if id == m.currentSession {
			m.sessionIdx = i
			found = true
			break
		}
	}
	if !found {
		m.sessionIdx = 0
		m.currentSession = m.sessionList[0]
	}
}

func (m Model) View() tea.View {
	if m.quitting && m.shutdownMsg != "" {
		return tea.NewView(m.shutdownMsg + "\n")
	}

	var b strings.Builder
	sess := m.store.GetSession(m.currentSession)
	inflight := m.store.GetInFlight()

	// Session pane
	b.WriteString(m.renderSessionPane(sess, inflight))
	b.WriteString("\n")

	// Request log pane
	b.WriteString(m.renderRequestLog(sess, inflight))
	b.WriteString("\n")

	// Aggregate pane
	b.WriteString(m.renderAggregatePane())

	return tea.NewView(b.String())
}

func (m Model) renderSessionPane(sess *store.Session, inflight map[uint64]store.InFlightReq) string {
	var b strings.Builder
	uptime := time.Since(m.startTime).Truncate(time.Second)

	sessionLabel := m.currentSession
	if len(m.sessionList) > 1 {
		sessionLabel = fmt.Sprintf("%s [%d/%d]", m.currentSession, m.sessionIdx+1, len(m.sessionList))
	}

	b.WriteString(paneHeaderStyle.Render(fmt.Sprintf(" Session: %s ", sessionLabel)))
	b.WriteString(statLabelStyle.Render(fmt.Sprintf("  Uptime: %s", uptime)))
	b.WriteString("\n")

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
	totalTok := inputTok + outputTok + cacheRead + cacheCreate

	tpm := m.store.CalculateTPM(m.currentSession)
	tpmStr := "--"
	if tpm > 0 {
		tpmStr = fmt.Sprintf("%.0f", tpm)
	}

	avgLatency := "--"
	if reqCount > 0 {
		avgLatency = fmt.Sprintf("%.1fs", (totalLatency / time.Duration(reqCount)).Seconds())
	}

	b.WriteString(fmt.Sprintf(" In: %s  Out: %s  Cache-R: %s  Cache-W: %s\n",
		statValueStyle.Render(formatNum(inputTok)),
		statValueStyle.Render(formatNum(outputTok)),
		statValueStyle.Render(formatNum(cacheRead)),
		statValueStyle.Render(formatNum(cacheCreate)),
	))
	b.WriteString(fmt.Sprintf(" Total: %s tok  TPM: %s  Reqs: %s  Avg latency: %s\n",
		statValueStyle.Render(formatNum(totalTok)),
		tpmStyle.Render(tpmStr),
		statValueStyle.Render(fmt.Sprintf("%d", reqCount)),
		statValueStyle.Render(avgLatency),
	))
	return borderStyle.Width(max(m.width-2, 60)).Render(b.String())
}

func (m Model) renderRequestLog(sess *store.Session, inflight map[uint64]store.InFlightReq) string {
	var b strings.Builder
	b.WriteString(paneHeaderStyle.Render(" Recent Requests "))
	b.WriteString("\n")

	var rows []string

	// Add inflight requests
	for _, inf := range inflight {
		if inf.SessionID == m.currentSession {
			elapsed := time.Since(inf.StartTime).Truncate(100 * time.Millisecond)
			row := inflightRowStyle.Render(fmt.Sprintf(" %s  %s  in-flight  %s...",
				inf.StartTime.Format("15:04:05"), inf.Endpoint, elapsed))
			rows = append(rows, row)
		}
	}

	// Add completed requests (newest first)
	if sess != nil {
		for i := len(sess.Requests) - 1; i >= 0; i-- {
			r := sess.Requests[i]
			latency := r.EndTime.Sub(r.StartTime).Truncate(100 * time.Millisecond)

			if r.StatusCode != 200 || r.HasError {
				detail := fmt.Sprintf("%d", r.StatusCode)
				if r.HasError {
					detail = "ERR"
				}
				if r.RetryAfter != "" {
					detail += fmt.Sprintf(" retry:%s", r.RetryAfter)
				}
				row := errorRowStyle.Render(fmt.Sprintf(" %s  %s  %-28s %s  %s",
					r.StartTime.Format("15:04:05"), r.Endpoint, r.Model, detail, latency))
				rows = append(rows, row)
			} else {
				row := requestRowStyle.Render(fmt.Sprintf(" %s  %s  %-28s in:%-5d out:%-5d cache:%-5d %s",
					r.StartTime.Format("15:04:05"), r.Endpoint, r.Model,
					r.InputTokens, r.OutputTokens, r.CacheRead, latency))
				rows = append(rows, row)
			}
		}
	}

	// Apply scroll offset and limit
	if m.scrollOffset >= len(rows) {
		m.scrollOffset = max(len(rows)-1, 0)
	}
	end := min(m.scrollOffset+m.maxLogRows, len(rows))
	visible := rows[m.scrollOffset:end]

	if len(visible) == 0 {
		b.WriteString(statLabelStyle.Render(" (no requests yet)\n"))
	} else {
		for _, row := range visible {
			b.WriteString(row + "\n")
		}
	}

	return b.String()
}

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

	tpm := m.store.CalculateAggregateTPM()
	tpmStr := "--"
	if tpm > 0 {
		tpmStr = fmt.Sprintf("%.0f", tpm)
	}

	uptime := time.Since(m.startTime).Truncate(time.Second)
	b.WriteString(fmt.Sprintf(" Sessions: %s  Total: %s tok  TPM: %s\n",
		statValueStyle.Render(fmt.Sprintf("%d", len(sessions))),
		statValueStyle.Render(formatNum(totalTok)),
		tpmStyle.Render(tpmStr),
	))
	b.WriteString(fmt.Sprintf(" Requests: %s  Uptime: %s\n",
		statValueStyle.Render(fmt.Sprintf("%d", totalReqs)),
		statValueStyle.Render(uptime.String()),
	))

	return aggregateStyle.Width(max(m.width-2, 60)).Render(
		paneHeaderStyle.Render(" Aggregate (All Sessions) ") + "\n" + b.String(),
	)
}

func formatNum(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./tui/`
Expected: Clean build.

- [ ] **Step 3: Commit**

```bash
git add tui/
git commit -m "feat: add bubbletea TUI with session pane, request log, and aggregate view"
```

---

### Task 9: CLI Query Mode

**Files:**
- Create: `query/query.go`
- Create: `query/query_test.go`

- [ ] **Step 1: Write failing tests for query formatting**

```go
// query/query_test.go
package query

import (
	"bytes"
	"claude-proxy/db"
	"claude-proxy/store"
	"testing"
	"time"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	d.InsertRecord(store.RequestRecord{
		StartTime: now.Add(-10 * time.Second), EndTime: now,
		Model: "claude-sonnet-4-20250514", InputTokens: 1000, OutputTokens: 500,
		SessionID: "s1", StatusCode: 200, Endpoint: "/v1/messages",
		TTFT: 100 * time.Millisecond,
	})
	d.InsertRecord(store.RequestRecord{
		StartTime: now, EndTime: now.Add(time.Second),
		SessionID: "s1", StatusCode: 429, RetryAfter: "30",
	})
	return d
}

func TestRunQuerySessions(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()

	var buf bytes.Buffer
	err := RunQuery(d, "sessions", Opts{Writer: &buf})
	if err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("s1")) {
		t.Fatalf("expected 's1' in output, got: %s", output)
	}
}

func TestRunQueryThrottle(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()

	var buf bytes.Buffer
	err := RunQuery(d, "throttle", Opts{Writer: &buf, TTFTThreshold: 5000})
	if err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("429")) {
		t.Fatalf("expected '429' in output, got: %s", output)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./query/ -v`
Expected: Compilation errors.

- [ ] **Step 3: Implement query runner**

```go
// query/query.go
package query

import (
	"claude-proxy/db"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

type Opts struct {
	From          string
	To            string
	SessionID     string
	TTFTThreshold int
	Writer        io.Writer
}

func RunQuery(d *db.DB, queryType string, opts Opts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	switch queryType {
	case "sessions":
		return runSessions(d, w)
	case "requests":
		return runRequests(d, w, opts)
	case "throttle":
		return runThrottle(d, w, opts)
	case "summary":
		return runSummary(d, w, opts)
	default:
		return fmt.Errorf("unknown query type: %s (valid: sessions, requests, throttle, summary)", queryType)
	}
}

func runSessions(d *db.DB, w io.Writer) error {
	sessions, err := d.QuerySessions()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tREQUESTS\tTOTAL TOKENS\tFIRST SEEN\tLAST SEEN")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n",
			s.ID, s.RequestCount, s.TotalTokens,
			s.FirstSeen.Format("2006-01-02 15:04:05"),
			s.LastSeen.Format("2006-01-02 15:04:05"))
	}
	return tw.Flush()
}

func runRequests(d *db.DB, w io.Writer, opts Opts) error {
	rows, err := d.QueryRequests(opts.From, opts.To, opts.SessionID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tMODEL\tSTATUS\tIN\tOUT\tCACHE\tTTFT\tLATENCY")
	for _, r := range rows {
		latency := r.EndTime.Sub(r.StartTime).Truncate(100 * time.Millisecond)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%dms\t%s\n",
			r.StartTime.Format("15:04:05"), r.SessionID, r.Model, r.StatusCode,
			r.InputTokens, r.OutputTokens, r.CacheRead,
			r.TTFT.Milliseconds(), latency)
	}
	return tw.Flush()
}

func runThrottle(d *db.DB, w io.Writer, opts Opts) error {
	threshold := opts.TTFTThreshold
	if threshold == 0 {
		threshold = 5000
	}
	rows, err := d.QueryThrottle(threshold, opts.From, opts.To, opts.SessionID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tMODEL\tSTATUS\tTTFT\tRETRY-AFTER\tREQ-REM\tTOK-REM\tERROR")
	for _, r := range rows {
		errStr := ""
		if r.HasError {
			errStr = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%dms\t%s\t%d\t%d\t%s\n",
			r.StartTime.Format("15:04:05"), r.SessionID, r.Model, r.StatusCode,
			r.TTFT.Milliseconds(), r.RetryAfter,
			r.RateLimitReqRemaining, r.RateLimitTokRemaining, errStr)
	}
	return tw.Flush()
}

func runSummary(d *db.DB, w io.Writer, opts Opts) error {
	summary, err := d.QuerySummary(opts.From, opts.To)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Total Requests:   %v\n", summary["total_requests"])
	fmt.Fprintf(w, "Total Tokens:     %v\n", summary["total_tokens"])
	fmt.Fprintf(w, "Sessions:         %v\n", summary["session_count"])
	fmt.Fprintf(w, "Avg TTFT:         %.0fms\n", summary["avg_ttft_ms"])
	fmt.Fprintf(w, "Throttle Events:  %v\n", summary["throttle_events"])
	return nil
}
```

Note: Add `"time"` to the imports in `query.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./query/ -v`
Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add query/
git commit -m "feat: add CLI query mode for sessions, requests, throttle, and summary"
```

---

### Task 10: Main Wiring

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Implement main.go with flag parsing, wiring, and startup/shutdown**

```go
// main.go
package main

import (
	"claude-proxy/db"
	"claude-proxy/query"
	"claude-proxy/proxy"
	"claude-proxy/store"
	"claude-proxy/tui"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
)

func main() {
	port := flag.Int("port", 8076, "Proxy listen port")
	upstream := flag.String("upstream", "https://api.anthropic.com", "Upstream API URL")
	session := flag.String("session", "default", "Session name")
	dbPath := flag.String("db", defaultDBPath(), "SQLite database path")
	queryMode := flag.String("query", "", "Query mode: sessions, requests, throttle, summary")
	from := flag.String("from", "", "Start time filter for queries")
	to := flag.String("to", "", "End time filter for queries")
	last := flag.String("last", "", "Relative time filter (e.g., 24h, 7d)")
	ttftThreshold := flag.Int("ttft-threshold", 5000, "TTFT threshold in ms for throttle queries")
	flag.Parse()

	// Resolve --last to --from
	if *last != "" && *from == "" {
		dur, err := time.ParseDuration(*last)
		if err != nil {
			log.Fatalf("invalid --last value: %v", err)
		}
		t := time.Now().Add(-dur).UTC().Format("2006-01-02 15:04:05")
		from = &t
	}

	// Query mode — no proxy, just query DB and exit
	if *queryMode != "" {
		d, err := db.Open(*dbPath)
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer d.Close()

		err = query.RunQuery(d, *queryMode, query.Opts{
			From:          *from,
			To:            *to,
			TTFTThreshold: *ttftThreshold,
		})
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	// Proxy mode
	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	// Ensure DB directory exists
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	d, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)

	// DB writer goroutine
	dbDone := make(chan struct{})
	go func() {
		defer close(dbDone)
		for rec := range dbChan {
			if err := d.InsertRecord(rec); err != nil {
				// Log to file, not stdout
			}
		}
	}()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: *session,
		Store:       st,
		DBChan:      dbChan,
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: p,
	}

	// Start HTTP server
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Set up debug logging for bubbletea
	f, err := tea.LogToFile("claude-proxy-debug.log", "claude-proxy")
	if err != nil {
		log.Fatalf("log to file: %v", err)
	}
	defer f.Close()

	// Run TUI
	model := tui.NewModel(st, *session)
	prog := tea.NewProgram(model)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}

	// Shutdown sequence
	// 1. Graceful HTTP shutdown (wait for in-flight requests)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// 2. Close DB write channel and drain
	close(dbChan)
	<-dbDone

	// 3. Close database
	d.Close()
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude-proxy-data.db"
	}
	return filepath.Join(home, ".claude-proxy", "data.db")
}
```

- [ ] **Step 2: Verify build**

Run: `go build -o claude-proxy .`
Expected: Clean build, `claude-proxy` binary created.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: wire everything together in main.go with startup/shutdown orchestration"
```

---

### Task 11: Integration Test

**Files:**
- Create: `integration_test.go`

- [ ] **Step 1: Write integration test using httptest**

```go
// integration_test.go
package main

import (
	"claude-proxy/proxy"
	"claude-proxy/store"
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
	// Mock upstream API that returns a JSON response with usage
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

	// Drain dbChan in background
	go func() { for range dbChan {} }()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: "test",
		Store:       st,
		DBChan:      dbChan,
	})

	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	// Send request through proxy
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
		t.Fatalf("response body not forwarded: %s", string(body))
	}

	// Wait for parser goroutine to finish
	time.Sleep(100 * time.Millisecond)

	sess := st.GetSession("test")
	if sess == nil {
		t.Fatal("no session found")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(sess.Requests))
	}
	r := sess.Requests[0]
	if r.InputTokens != 1000 {
		t.Fatalf("expected 1000 input tokens, got %d", r.InputTokens)
	}
	if r.OutputTokens != 500 {
		t.Fatalf("expected 500 output tokens, got %d", r.OutputTokens)
	}
	if r.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", r.Model)
	}

	close(dbChan)
}

func TestProxyHandlesSSEStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server does not support flushing")
		}
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
	go func() { for range dbChan {} }()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: "sse-test",
		Store:       st,
		DBChan:      dbChan,
	})

	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model": "claude-sonnet-4-20250514", "stream": true}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	sess := st.GetSession("sse-test")
	if sess == nil {
		t.Fatal("no session found")
	}
	if len(sess.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(sess.Requests))
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
	go func() { for range dbChan {} }()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: "throttle-test",
		Store:       st,
		DBChan:      dbChan,
	})

	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)

	sess := st.GetSession("throttle-test")
	if sess == nil {
		t.Fatal("no session found")
	}
	r := sess.Requests[0]
	if r.StatusCode != 429 {
		t.Fatalf("expected status 429, got %d", r.StatusCode)
	}
	if r.RetryAfter != "30" {
		t.Fatalf("expected retry-after '30', got '%s'", r.RetryAfter)
	}

	close(dbChan)
}
```

- [ ] **Step 2: Run integration tests**

Run: `go test -v -run TestProxy`
Expected: All 3 integration tests PASS.

- [ ] **Step 3: Run all tests**

Run: `go test ./... -v`
Expected: All tests across all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add integration_test.go
git commit -m "feat: add integration tests for JSON, SSE, and 429 proxy flows"
```

---

### Task 12: Build and Manual Test

**Files:** None new — verification only.

- [ ] **Step 1: Build the binary**

Run: `go build -o claude-proxy .`
Expected: Clean build.

- [ ] **Step 2: Run the proxy**

Run: `./claude-proxy --port 8076`
Expected: TUI launches showing empty session. Press `q` to quit cleanly.

- [ ] **Step 3: Test query mode**

Run: `./claude-proxy --query summary --last 24h`
Expected: Shows summary with 0 requests (empty DB).

- [ ] **Step 4: Final commit**

```bash
git add -A
git commit -m "feat: claude-proxy v1 complete — proxy, TUI, SQLite persistence, CLI queries"
```

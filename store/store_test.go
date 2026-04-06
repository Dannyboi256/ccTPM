package store

import (
	"testing"
	"time"
)

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
		StartTime:    time.Now().Add(-2 * time.Second),
		EndTime:      time.Now(),
		Model:        "claude-sonnet-4-20250514",
		InputTokens:  1000,
		OutputTokens: 500,
		Endpoint:     "/v1/messages",
		StatusCode:   200,
		TTFT:         100 * time.Millisecond,
		SessionID:    "test-session",
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
		StartTime:  time.Now(),
		EndTime:    time.Now(),
		SessionID:  "new-session",
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
	now := time.Now()
	s.AddRecord(newTestRecord(
		withSession("s1"),
		withEndTime(now.Add(-10*time.Second)),
		withTokens(100, 0, 0, 0),
	))
	s.AddInFlight("s1", "/v1/messages")
	got := s.RollingITPM("s1", now)
	if got != 100 {
		t.Fatalf("expected 100 (completed record counts, in-flight does not), got %v", got)
	}
}

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

func TestGetActiveTime(t *testing.T) {
	s := NewStore()
	now := time.Now()

	// No session -> 0
	if at := s.GetActiveTime("nonexistent"); at != 0 {
		t.Fatalf("expected 0 for nonexistent session, got %v", at)
	}

	// Single 30s request -> 30s active time
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		SessionID: "s1", StatusCode: 200,
	})
	at := s.GetActiveTime("s1")
	if at < 29*time.Second || at > 31*time.Second {
		t.Fatalf("expected ~30s active time, got %v", at)
	}

	// Two concurrent 30s requests -> still 30s (merged)
	s.AddRecord(RequestRecord{
		StartTime: now.Add(-30 * time.Second), EndTime: now,
		SessionID: "s1", StatusCode: 200,
	})
	at = s.GetActiveTime("s1")
	if at < 29*time.Second || at > 31*time.Second {
		t.Fatalf("expected ~30s active time with concurrent requests, got %v", at)
	}
}

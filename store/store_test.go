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

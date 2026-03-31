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

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

	rows, err := d.QueryRequests("", "", "", 0)
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

	sessions, err := d.QuerySessions(0)
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

	rows, err := d.QueryThrottle(5000, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Should match: 429, SSE error, high TTFT (not the normal one)
	if len(rows) != 3 {
		t.Fatalf("expected 3 throttle events, got %d", len(rows))
	}
}

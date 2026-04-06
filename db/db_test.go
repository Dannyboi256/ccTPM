package db

import (
	"claude-proxy/store"
	"database/sql"
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
		"end_time_epoch",
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

func TestSchemaMigration_PreservesExistingData(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert a row using only old columns to simulate pre-migration data.
	_, err = d.conn.Exec(`INSERT INTO requests (
		session_id, start_time, end_time, model, endpoint, status_code,
		input_tokens, output_tokens, cache_creation, cache_read,
		ttft_ms, retry_after, ratelimit_req_remaining, ratelimit_tok_remaining, has_error
	) VALUES ('s1', ?, ?, 'm', '/v1/messages', 200, 100, 50, 0, 0, 0, '', -1, -1, 0)`,
		time.Now().Add(-5*time.Second).UTC(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	// Run createSchema again -- should be idempotent and not damage the row.
	if err := d.createSchema(); err != nil {
		t.Fatalf("second createSchema failed: %v", err)
	}

	rows, err := d.QueryRequests("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after idempotent migration, got %d", len(rows))
	}
	if rows[0].SessionID != "s1" || rows[0].InputTokens != 100 {
		t.Fatalf("old data corrupted: %+v", rows[0])
	}
}

func TestSchemaMigration_Idempotent(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Run createSchema a second time -- should not error
	if err := d.createSchema(); err != nil {
		t.Fatalf("second createSchema call failed: %v", err)
	}
}

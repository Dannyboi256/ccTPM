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

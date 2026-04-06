package query

import (
	"bytes"
	"claude-proxy/db"
	"claude-proxy/store"
	"strings"
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
		From:   base.Add(-1 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		To:     base.Add(5 * time.Minute).Local().Format("2006-01-02 15:04:05"),
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
		From:   base.Add(-1 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		To:     base.Add(5 * time.Minute).Local().Format("2006-01-02 15:04:05"),
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
		From:    base.Add(-1 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		To:      base.Add(5 * time.Minute).Local().Format("2006-01-02 15:04:05"),
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

func TestRunTPM_GroupBySessionRequiresPeak(t *testing.T) {
	// --group-by session only makes sense with --peak. Bucketed mode without --peak
	// aggregates across all sessions into time buckets, so session grouping is ignored.
	// This test documents that behavior: passing GroupBy without Peak is accepted
	// (GroupBy is silently unused in bucketed mode). If we later decide to reject it,
	// this test will fail and force an explicit rules update.
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	_ = d.InsertRecord(store.RequestRecord{
		SessionID:   "s1",
		StartTime:   time.Now().Add(-5 * time.Second),
		EndTime:     time.Now(),
		InputTokens: 100, StatusCode: 200,
	})

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		GroupBy: "session", // with no --peak, should be silently ignored
		Writer:  &buf,
	})
	if err != nil {
		t.Fatalf("expected no error when GroupBy is set without Peak, got: %v", err)
	}
	if !strings.Contains(buf.String(), "BUCKET_START") {
		t.Errorf("expected bucketed output (GroupBy ignored), got %q", buf.String())
	}
}

func TestRunTPM_LocalTimeRange(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert a record with a known EndTime
	now := time.Now()
	_ = d.InsertRecord(store.RequestRecord{
		SessionID:   "s1",
		StartTime:   now.Add(-5 * time.Second),
		EndTime:     now,
		InputTokens: 500,
		StatusCode:  200,
	})

	// Query using local-time formatted strings that match the record
	from := now.Add(-1 * time.Minute).Format("2006-01-02 15:04:05")
	to := now.Add(1 * time.Minute).Format("2006-01-02 15:04:05")

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:   from,
		To:     to,
		Bucket: 60,
		Writer: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "500") {
		t.Errorf("expected record to appear in local-time range, got %q", buf.String())
	}
}

func TestRunTPM_SessionFilter(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	base := time.Date(2026, 4, 5, 14, 0, 0, 0, time.UTC)
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "wanted", StartTime: base, EndTime: base.Add(5 * time.Second),
		InputTokens: 1000, StatusCode: 200,
	})
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "unwanted", StartTime: base, EndTime: base.Add(5 * time.Second),
		InputTokens: 9999, StatusCode: 200,
	})

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:      base.Add(-1 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		To:        base.Add(5 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		Bucket:    60,
		SessionID: "wanted",
		Writer:    &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1000") {
		t.Errorf("expected wanted session ITPM=1000, got %q", out)
	}
	if strings.Contains(out, "9999") || strings.Contains(out, "10999") {
		t.Errorf("unwanted session should be filtered out, got %q", out)
	}
}

func TestRunTPM_BucketedDefaultLimitIsUnlimited(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert 15 records, each one minute apart — should produce 15 distinct 1-minute buckets
	base := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 15; i++ {
		end := base.Add(time.Duration(i) * time.Minute)
		_ = d.InsertRecord(store.RequestRecord{
			SessionID:   "s1",
			StartTime:   end.Add(-5 * time.Second),
			EndTime:     end,
			InputTokens: 100 * (i + 1), // distinct per bucket
			StatusCode:  200,
		})
	}

	var buf bytes.Buffer
	err = RunQuery(d, "tpm", Opts{
		From:   base.Add(-1 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		To:     base.Add(20 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		Bucket: 60, // 1-minute buckets
		Limit:  0,  // default — should be unlimited for tpm bucketed
		Writer: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Count data rows (skip the header line)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	dataRows := len(lines) - 1
	if dataRows < 15 {
		t.Errorf("expected at least 15 bucket rows with unlimited default, got %d (output: %q)", dataRows, buf.String())
	}
}

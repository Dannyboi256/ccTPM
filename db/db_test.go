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
	if got.ITokensRemaining == nil || *got.ITokensRemaining != 448500 {
		t.Errorf("ITokensRemaining wrong: %v", got.ITokensRemaining)
	}
	if got.ITokensReset != "2026-04-05T14:30:00Z" {
		t.Errorf("ITokensReset wrong: %q", got.ITokensReset)
	}
	if got.Unified5hUtil == nil || *got.Unified5hUtil != 0.0184 {
		t.Errorf("Unified5hUtil wrong: %v", got.Unified5hUtil)
	}
	if got.Unified5hReset == nil || *got.Unified5hReset != 1712345678 {
		t.Errorf("Unified5hReset wrong: %v", got.Unified5hReset)
	}
	if got.UnifiedStatus != "allowed" {
		t.Errorf("UnifiedStatus wrong: %q", got.UnifiedStatus)
	}
	if got.UnifiedReprClaim != "five_hour" {
		t.Errorf("UnifiedReprClaim wrong: %q", got.UnifiedReprClaim)
	}
}

func TestInsertAndQueryRecord_AllHeadersNil(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec := store.RequestRecord{
		SessionID:  "s1",
		StartTime:  time.Now().Add(-1 * time.Second),
		EndTime:    time.Now(),
		Model:      "m",
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
	if got.OTokensLimit != nil {
		t.Errorf("expected nil OTokensLimit, got %v", *got.OTokensLimit)
	}
	if got.RPMLimit != nil {
		t.Errorf("expected nil RPMLimit, got %v", *got.RPMLimit)
	}
	if got.Unified5hUtil != nil {
		t.Errorf("expected nil Unified5hUtil, got %v", *got.Unified5hUtil)
	}
	if got.Unified7dUtil != nil {
		t.Errorf("expected nil Unified7dUtil, got %v", *got.Unified7dUtil)
	}
	if got.UnifiedStatus != "" {
		t.Errorf("expected empty UnifiedStatus, got %q", got.UnifiedStatus)
	}
	if got.UnifiedReprClaim != "" {
		t.Errorf("expected empty UnifiedReprClaim, got %q", got.UnifiedReprClaim)
	}
}

func TestSchemaMigration_BackfillsEndTimeEpoch(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert a record — end_time_epoch will be populated by InsertRecord.
	// To simulate a pre-migration row, manually NULL out end_time_epoch.
	now := time.Now()
	rec := store.RequestRecord{
		SessionID:   "s1",
		StartTime:   now.Add(-5 * time.Second),
		EndTime:     now,
		InputTokens: 100,
		StatusCode:  200,
	}
	if err := d.InsertRecord(rec); err != nil {
		t.Fatal(err)
	}
	// Simulate pre-migration state: NULL out the epoch
	if _, err := d.conn.Exec(`UPDATE requests SET end_time_epoch = NULL`); err != nil {
		t.Fatal(err)
	}

	// Re-run createSchema — the backfill should populate end_time_epoch
	if err := d.createSchema(); err != nil {
		t.Fatal(err)
	}

	var epoch sql.NullInt64
	if err := d.conn.QueryRow(`SELECT end_time_epoch FROM requests WHERE session_id = 's1'`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if !epoch.Valid {
		t.Fatal("expected end_time_epoch to be backfilled, got NULL")
	}
	// The epoch should be close to now.Unix()
	diff := now.Unix() - epoch.Int64
	if diff < -2 || diff > 2 {
		t.Errorf("backfilled epoch off by %d seconds", diff)
	}
}

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
	buckets, err := d.QueryTPMBuckets(from, to, 60, "")
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

	buckets, err := d.QueryTPMBuckets(time.Now().Add(-1*time.Hour), time.Now(), 60, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Fatalf("expected empty result, got %d buckets", len(buckets))
	}
}

func TestQueryTPMBuckets_BucketBoundaryGrouping(t *testing.T) {
	// A record ending at 14:59:59 must group into the 14:00 hour bucket (not 15:00)
	// when bucketSeconds=3600. A record at 15:00:00 must group into the 15:00 bucket.
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	at1459 := time.Date(2026, 4, 5, 14, 59, 59, 0, time.UTC)
	at1500 := time.Date(2026, 4, 5, 15, 0, 0, 0, time.UTC)
	at1501 := time.Date(2026, 4, 5, 15, 0, 1, 0, time.UTC)

	_ = d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: at1459.Add(-1 * time.Second), EndTime: at1459, InputTokens: 100, StatusCode: 200})
	_ = d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: at1500.Add(-1 * time.Second), EndTime: at1500, InputTokens: 200, StatusCode: 200})
	_ = d.InsertRecord(store.RequestRecord{SessionID: "s1", StartTime: at1501.Add(-1 * time.Second), EndTime: at1501, InputTokens: 300, StatusCode: 200})

	buckets, err := d.QueryTPMBuckets(at1459.Add(-1*time.Minute), at1501.Add(1*time.Minute), 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 hour buckets, got %d: %+v", len(buckets), buckets)
	}
	// First bucket starts at 14:00:00 UTC, second at 15:00:00 UTC
	if buckets[0].BucketStart.Hour() != 14 {
		t.Errorf("expected first bucket at hour 14, got %v", buckets[0].BucketStart)
	}
	if buckets[1].BucketStart.Hour() != 15 {
		t.Errorf("expected second bucket at hour 15, got %v", buckets[1].BucketStart)
	}
	// 14:59:59 record (ITPM 100) must be in the first bucket, not the second.
	if buckets[0].MaxITPM != 100 {
		t.Errorf("expected 14:00 bucket MaxITPM=100 (from the 14:59:59 record), got %v", buckets[0].MaxITPM)
	}
	// 15:00:00 and 15:00:01 both fall in minute 15:00, so their itpm combines: 200+300=500
	if buckets[1].MaxITPM != 500 {
		t.Errorf("expected 15:00 bucket MaxITPM=500 (combined minute 15:00), got %v", buckets[1].MaxITPM)
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

	peak, err := d.QueryTPMPeak(base.Add(-1*time.Minute), base.Add(10*time.Minute), "", "")
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

func TestQueryTPMPeak_UngroupedAggregatesCrossSessions(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Two sessions with records in the SAME minute
	base := time.Date(2026, 4, 5, 14, 0, 5, 0, time.UTC)
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "s1", StartTime: base.Add(-1 * time.Second), EndTime: base,
		InputTokens: 1000, OutputTokens: 200, StatusCode: 200,
	})
	_ = d.InsertRecord(store.RequestRecord{
		SessionID: "s2", StartTime: base.Add(-1 * time.Second), EndTime: base,
		InputTokens: 500, OutputTokens: 100, StatusCode: 200,
	})

	peak, err := d.QueryTPMPeak(base.Add(-1*time.Minute), base.Add(1*time.Minute), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(peak) != 1 {
		t.Fatalf("expected 1 peak row, got %d", len(peak))
	}
	// Ungrouped peak should reflect the COMBINED minute: 1000+500=1500 ITPM, not max(1000,500)=1000
	if peak[0].MaxITPM != 1500 {
		t.Errorf("expected combined MaxITPM=1500, got %v (cross-session aggregation broken)", peak[0].MaxITPM)
	}
	if peak[0].MaxOTPM != 300 {
		t.Errorf("expected combined MaxOTPM=300, got %v", peak[0].MaxOTPM)
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

	peak, err := d.QueryTPMPeak(base.Add(-1*time.Minute), base.Add(10*time.Minute), "session", "")
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

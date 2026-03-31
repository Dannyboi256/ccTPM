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

func (d *DB) QueryRequests(from, to, sessionID string, limit int) ([]store.RequestRecord, error) {
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
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (d *DB) QuerySessions(limit int) ([]SessionSummary, error) {
	q := `SELECT session_id,
		COUNT(*) as request_count,
		SUM(input_tokens + output_tokens + cache_creation + cache_read) as total_tokens,
		MIN(start_time) as first_seen,
		MAX(end_time) as last_seen
		FROM requests
		GROUP BY session_id
		ORDER BY last_seen DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var firstSeen, lastSeen string
		if err := rows.Scan(&s.ID, &s.RequestCount, &s.TotalTokens, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		s.FirstSeen = parseTime(firstSeen)
		s.LastSeen = parseTime(lastSeen)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (d *DB) QueryThrottle(ttftThresholdMs int, from, to, sessionID string, limit int) ([]store.RequestRecord, error) {
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
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

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
		COALESCE(SUM(CASE WHEN status_code IN (429, 529) OR has_error = 1 THEN 1 ELSE 0 END), 0) as throttle_events
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
		"total_requests":  totalReqs,
		"total_tokens":    totalTokens,
		"session_count":   sessionCount,
		"avg_ttft_ms":     avgTTFT,
		"throttle_events": throttleEvents,
	}, nil
}

// timeFormats lists the datetime layouts SQLite may return.
var timeFormats = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

func parseTime(s string) time.Time {
	for _, layout := range timeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func scanRecords(rows *sql.Rows) ([]store.RequestRecord, error) {
	var records []store.RequestRecord
	for rows.Next() {
		var r store.RequestRecord
		var ttftMs int64
		var hasErr int
		var startTime, endTime string
		if err := rows.Scan(
			&r.SessionID, &startTime, &endTime, &r.Model, &r.Endpoint, &r.StatusCode,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreation, &r.CacheRead,
			&ttftMs, &r.RetryAfter, &r.RateLimitReqRemaining, &r.RateLimitTokRemaining, &hasErr,
		); err != nil {
			return nil, err
		}
		r.StartTime = parseTime(startTime)
		r.EndTime = parseTime(endTime)
		r.TTFT = time.Duration(ttftMs) * time.Millisecond
		r.HasError = hasErr != 0
		records = append(records, r)
	}
	return records, rows.Err()
}

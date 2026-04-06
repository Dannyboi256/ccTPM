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

	// Per-direction API-key rate-limit headers (anthropic-ratelimit-{input,output}-tokens-*)
	ITokensLimit     *int
	ITokensRemaining *int
	ITokensReset     string // RFC3339 as emitted by API
	OTokensLimit     *int
	OTokensRemaining *int
	OTokensReset     string

	// Per-key request-rate headers (anthropic-ratelimit-requests-*)
	RPMLimit     *int
	RPMRemaining *int
	RPMReset     string

	// Unified OAuth rate-limit headers (anthropic-ratelimit-unified-*)
	Unified5hUtil    *float64
	Unified5hReset   *int64 // unix seconds
	Unified5hStatus  string
	Unified7dUtil    *float64
	Unified7dReset   *int64 // unix seconds
	Unified7dStatus  string
	UnifiedStatus    string // "allowed" | "rate_limited"
	UnifiedReprClaim string // e.g., "five_hour"
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
	if len(sess.Requests) > 1000 {
		sess.Requests = sess.Requests[len(sess.Requests)-1000:]
	}
}

func (s *Store) Prune(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for id, sess := range s.sessions {
		if sess.LastSeen.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
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

// GetActiveTime returns the merged-interval active time for a specific session.
// Returns 0 if the session doesn't exist or has no requests.
func (s *Store) GetActiveTime(sessionID string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.sessions[sessionID]; !ok {
		return 0
	}
	intervals := s.collectIntervals(sessionID)
	merged := mergeIntervals(intervals)
	return sumDurations(merged)
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

// RollingITPM returns the rolling 60-second ITPM (input tokens per minute) for a session.
// A record contributes if its EndTime falls in (now-60s, now].
// ITPM = Σ(InputTokens + CacheCreation) — matches Anthropic's documented ITPM definition.
// CacheRead is deliberately excluded (modern models do not count it toward ITPM).
// In-flight requests are excluded because their token counts are not yet known.
func (s *Store) RollingITPM(sessionID string, now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			total += r.InputTokens + r.CacheCreation
		}
	}
	return float64(total)
}

// RollingOTPM returns the rolling 60-second OTPM (output tokens per minute) for a session.
// Uses end-time attribution. In-flight requests are excluded.
func (s *Store) RollingOTPM(sessionID string, now time.Time) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	var total int
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			total += r.OutputTokens
		}
	}
	return float64(total)
}

// RollingRPM returns the rolling 60-second RPM (requests per minute) for a session.
// Counts all records (including zero-token error responses) whose EndTime falls in the window.
func (s *Store) RollingRPM(sessionID string, now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0
	}
	windowStart := now.Add(-60 * time.Second)
	count := 0
	for _, r := range sess.Requests {
		if r.EndTime.After(windowStart) && !r.EndTime.After(now) {
			count++
		}
	}
	return count
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

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

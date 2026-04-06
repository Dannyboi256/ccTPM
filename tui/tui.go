package tui

import (
	"claude-proxy/store"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type tickMsg time.Time

type Model struct {
	store          *store.Store
	currentSession string
	sessionList    []string
	sessionIdx     int
	scrollOffset   int
	maxLogRows     int
	width          int
	height         int
	quitting       bool
	startTime      time.Time
	shutdownMsg    string
}

func NewModel(s *store.Store, initialSession string) Model {
	return Model{
		store:          s,
		currentSession: initialSession,
		sessionList:    []string{initialSession},
		maxLogRows:     10,
		startTime:      time.Now(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.maxLogRows = maxInt(m.height-12, 3)

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			inflight := m.store.InFlightCount()
			if inflight > 0 {
				m.shutdownMsg = fmt.Sprintf("waiting for %d in-flight request(s)...", inflight)
			}
			m.quitting = true
			return m, tea.Quit
		case "tab":
			if len(m.sessionList) > 1 {
				m.sessionIdx = (m.sessionIdx + 1) % len(m.sessionList)
				m.currentSession = m.sessionList[m.sessionIdx]
				m.scrollOffset = 0
			}
		case "j", "down":
			m.scrollOffset++
		case "k", "up":
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		}

	case tickMsg:
		m.refreshSessionList()
		// Compute current rolling metrics and update peaks for the visible session
		// and for the aggregate. Peaks are stored in the Store under store.mu.
		now := time.Now()
		itpm := m.store.RollingITPM(m.currentSession, now)
		otpm := m.store.RollingOTPM(m.currentSession, now)
		rpm := m.store.RollingRPM(m.currentSession, now)
		m.store.UpdateSessionPeaks(m.currentSession, itpm, otpm, rpm, now)

		aItpm := m.store.RollingAggregateITPM(now)
		aOtpm := m.store.RollingAggregateOTPM(now)
		aRpm := m.store.RollingAggregateRPM(now)
		m.store.UpdateAggregatePeaks(aItpm, aOtpm, aRpm, now)

		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})
	}
	return m, nil
}

func (m *Model) refreshSessionList() {
	sessions := m.store.GetAllSessions()
	m.sessionList = make([]string, len(sessions))
	for i, s := range sessions {
		m.sessionList[i] = s.ID
	}
	if len(m.sessionList) == 0 {
		m.sessionList = []string{m.currentSession}
	}
	found := false
	for i, id := range m.sessionList {
		if id == m.currentSession {
			m.sessionIdx = i
			found = true
			break
		}
	}
	if !found {
		m.sessionIdx = 0
		m.currentSession = m.sessionList[0]
	}
}

func (m Model) View() tea.View {
	if m.quitting && m.shutdownMsg != "" {
		return tea.NewView(m.shutdownMsg + "\n")
	}

	var b strings.Builder
	sess := m.store.GetSession(m.currentSession)
	inflight := m.store.GetInFlight()

	b.WriteString(m.renderSessionPane(sess, inflight))
	b.WriteString("\n")
	b.WriteString(m.renderRequestLog(sess, inflight))
	b.WriteString("\n")
	b.WriteString(m.renderAggregatePane())

	return tea.NewView(b.String())
}

func (m Model) renderSessionPane(sess *store.Session, inflight map[uint64]store.InFlightReq) string {
	var b strings.Builder
	uptime := time.Since(m.startTime).Truncate(time.Second)

	sessionLabel := m.currentSession
	if len(m.sessionList) > 1 {
		sessionLabel = fmt.Sprintf("%s [%d/%d]", m.currentSession, m.sessionIdx+1, len(m.sessionList))
	}

	activeTime := m.store.GetActiveTime(m.currentSession)
	activeStr := formatDuration(activeTime)
	b.WriteString(paneHeaderStyle.Render(fmt.Sprintf(" Session: %s ", sessionLabel)))
	b.WriteString(statLabelStyle.Render(fmt.Sprintf("  Duration: %s (active: %s)", uptime, activeStr)))
	b.WriteString("\n")

	now := time.Now()
	itpm := m.store.RollingITPM(m.currentSession, now)
	otpm := m.store.RollingOTPM(m.currentSession, now)
	rpm := m.store.RollingRPM(m.currentSession, now)
	peaks := m.store.GetSessionPeaks(m.currentSession)

	hasData := sess != nil && len(sess.Requests) > 0
	b.WriteString(fmt.Sprintf("  ITPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(itpm, hasData),
		formatRateFloat(peaks.MaxITPM, peaks.MaxITPM > 0),
		formatPeakTime(peaks.MaxITPMTime),
	))
	b.WriteString(fmt.Sprintf("  OTPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(otpm, hasData),
		formatRateFloat(peaks.MaxOTPM, peaks.MaxOTPM > 0),
		formatPeakTime(peaks.MaxOTPMTime),
	))
	b.WriteString(fmt.Sprintf("  RPM   |  current: %s     peak: %s  %s\n",
		formatRateInt(rpm, hasData),
		formatRateInt(peaks.MaxRPM, peaks.MaxRPM > 0),
		formatPeakTime(peaks.MaxRPMTime),
	))

	// Secondary totals line — cumulative counters for visibility
	var inputTok, outputTok, cacheRead, cacheCreate, reqCount int
	var totalLatency time.Duration
	if sess != nil {
		for _, r := range sess.Requests {
			inputTok += r.InputTokens
			outputTok += r.OutputTokens
			cacheRead += r.CacheRead
			cacheCreate += r.CacheCreation
			totalLatency += r.EndTime.Sub(r.StartTime)
		}
		reqCount = len(sess.Requests)
	}
	avgLatency := "--"
	if reqCount > 0 {
		avgLatency = fmt.Sprintf("%.1fs", (totalLatency / time.Duration(reqCount)).Seconds())
	}

	b.WriteString(fmt.Sprintf("  Totals: In %s  Out %s  Cache-R %s  Cache-W %s  Reqs %d  Avg %s\n",
		statValueStyle.Render(formatNum(inputTok)),
		statValueStyle.Render(formatNum(outputTok)),
		statValueStyle.Render(formatNum(cacheRead)),
		statValueStyle.Render(formatNum(cacheCreate)),
		reqCount,
		avgLatency,
	))

	return borderStyle.Width(maxInt(m.width-2, 60)).Render(b.String())
}

// formatRateFloat renders a float rate, returning "--" if there's no data yet.
func formatRateFloat(v float64, hasData bool) string {
	if !hasData {
		return "--"
	}
	return statValueStyle.Render(formatNum(int(v)))
}

// formatRateInt renders an integer rate, returning "--" if there's no data yet.
func formatRateInt(v int, hasData bool) string {
	if !hasData {
		return "--"
	}
	return statValueStyle.Render(formatNum(v))
}

// formatPeakTime returns "(HH:MM:SS)" for a non-zero time, or "" otherwise.
func formatPeakTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("(%s)", t.Format("15:04:05"))
}

func (m Model) renderRequestLog(sess *store.Session, inflight map[uint64]store.InFlightReq) string {
	var b strings.Builder
	b.WriteString(paneHeaderStyle.Render(" Recent Requests "))
	b.WriteString("\n")

	var rows []string

	for _, inf := range inflight {
		if inf.SessionID == m.currentSession {
			elapsed := time.Since(inf.StartTime).Truncate(100 * time.Millisecond)
			row := inflightRowStyle.Render(fmt.Sprintf(" %s  %s  in-flight  %s...",
				inf.StartTime.Format("15:04:05"), inf.Endpoint, elapsed))
			rows = append(rows, row)
		}
	}

	if sess != nil {
		startIdx := maxInt(0, len(sess.Requests)-10)
		for i := len(sess.Requests) - 1; i >= startIdx; i-- {
			r := sess.Requests[i]
			latency := r.EndTime.Sub(r.StartTime).Truncate(100 * time.Millisecond)

			if r.StatusCode != 200 || r.HasError {
				detail := fmt.Sprintf("%d", r.StatusCode)
				if r.HasError {
					detail = "ERR"
				}
				if r.RetryAfter != "" {
					detail += fmt.Sprintf(" retry:%s", r.RetryAfter)
				}
				row := errorRowStyle.Render(fmt.Sprintf(" %s  %s  %-28s %s  %s",
					r.StartTime.Format("15:04:05"), r.Endpoint, r.Model, detail, latency))
				rows = append(rows, row)
			} else {
				row := requestRowStyle.Render(fmt.Sprintf(" %s  %s  %-28s in:%-5d out:%-5d cache:%-5d %s",
					r.StartTime.Format("15:04:05"), r.Endpoint, r.Model,
					r.InputTokens, r.OutputTokens, r.CacheRead, latency))
				rows = append(rows, row)
			}
		}
	}

	offset := m.scrollOffset
	if offset >= len(rows) {
		offset = maxInt(len(rows)-1, 0)
	}
	end := minInt(offset+m.maxLogRows, len(rows))
	visible := rows[offset:end]

	if len(visible) == 0 {
		b.WriteString(statLabelStyle.Render(" (no requests yet)\n"))
	} else {
		for _, row := range visible {
			b.WriteString(row + "\n")
		}
	}

	return b.String()
}

func (m Model) renderAggregatePane() string {
	var b strings.Builder
	sessions := m.store.GetAllSessions()
	var totalReqs, totalTok int
	for _, s := range sessions {
		totalReqs += len(s.Requests)
		for _, r := range s.Requests {
			totalTok += r.InputTokens + r.OutputTokens + r.CacheRead + r.CacheCreation
		}
	}

	now := time.Now()
	aItpm := m.store.RollingAggregateITPM(now)
	aOtpm := m.store.RollingAggregateOTPM(now)
	aRpm := m.store.RollingAggregateRPM(now)
	peaks := m.store.GetAggregatePeaks()
	hasData := totalReqs > 0

	b.WriteString(fmt.Sprintf("  ITPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(aItpm, hasData),
		formatRateFloat(peaks.MaxITPM, peaks.MaxITPM > 0),
		formatPeakTime(peaks.MaxITPMTime),
	))
	b.WriteString(fmt.Sprintf("  OTPM  |  current: %s     peak: %s  %s\n",
		formatRateFloat(aOtpm, hasData),
		formatRateFloat(peaks.MaxOTPM, peaks.MaxOTPM > 0),
		formatPeakTime(peaks.MaxOTPMTime),
	))
	b.WriteString(fmt.Sprintf("  RPM   |  current: %s     peak: %s  %s\n",
		formatRateInt(aRpm, hasData),
		formatRateInt(peaks.MaxRPM, peaks.MaxRPM > 0),
		formatPeakTime(peaks.MaxRPMTime),
	))

	uptime := time.Since(m.startTime).Truncate(time.Second)
	b.WriteString(fmt.Sprintf("  Sessions: %s  Requests: %s  Total tok: %s  Uptime: %s\n",
		statValueStyle.Render(fmt.Sprintf("%d", len(sessions))),
		statValueStyle.Render(fmt.Sprintf("%d", totalReqs)),
		statValueStyle.Render(formatNum(totalTok)),
		statValueStyle.Render(uptime.String()),
	))

	return aggregateStyle.Width(maxInt(m.width-2, 60)).Render(
		paneHeaderStyle.Render(" Aggregate (All Sessions) ") + "\n" + b.String(),
	)
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %02ds", m, s)
}

func formatNum(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// main.go
package main

import (
	"claude-proxy/db"
	"claude-proxy/proxy"
	"claude-proxy/query"
	"claude-proxy/store"
	"claude-proxy/tui"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
)

func main() {
	port := flag.Int("port", 8076, "Proxy listen port")
	upstream := flag.String("upstream", "https://api.anthropic.com", "Upstream API URL")
	session := flag.String("session", "default", "Session name")
	dbPath := flag.String("db", defaultDBPath(), "SQLite database path")
	queryMode := flag.String("query", "", "Query mode: sessions, requests, throttle, summary")
	from := flag.String("from", "", "Start time filter for queries")
	to := flag.String("to", "", "End time filter for queries")
	last := flag.String("last", "", "Relative time filter (e.g., 24h, 7d)")
	ttftThreshold := flag.Int("ttft-threshold", 5000, "TTFT threshold in ms for throttle queries")
	limit := flag.Int("limit", 10, "Max rows to return in query mode (0 = unlimited)")
	flag.Parse()

	// Resolve --last to --from
	if *last != "" && *from == "" {
		dur, err := parseDuration(*last)
		if err != nil {
			log.Fatalf("invalid --last value: %v", err)
		}
		t := time.Now().Add(-dur).UTC().Format("2006-01-02 15:04:05")
		from = &t
	}

	// Query mode — no proxy, just query DB and exit
	if *queryMode != "" {
		d, err := db.Open(*dbPath)
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer d.Close()

		// Only filter by session if explicitly set (default "default" is for proxy mode)
		sessionFilter := ""
		if *session != "default" {
			sessionFilter = *session
		}
		err = query.RunQuery(d, *queryMode, query.Opts{
			From:          *from,
			To:            *to,
			TTFTThreshold: *ttftThreshold,
			Limit:         *limit,
			SessionID:     sessionFilter,
		})
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	// Proxy mode
	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	// Ensure DB directory exists
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	d, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	st := store.NewStore()
	dbChan := make(chan store.RequestRecord, 256)

	// DB writer goroutine
	dbDone := make(chan struct{})
	go func() {
		defer close(dbDone)
		for rec := range dbChan {
			if err := d.InsertRecord(rec); err != nil {
				log.Printf("db insert error: %v", err)
			}
		}
	}()

	// Periodically prune stale sessions from in-memory store
	pruneDone := make(chan struct{})
	go func() {
		defer close(pruneDone)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			st.Prune(1 * time.Hour)
		}
	}()

	p := proxy.NewProxy(proxy.Config{
		UpstreamURL: upstreamURL,
		SessionName: *session,
		Store:       st,
		DBChan:      dbChan,
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: p,
	}

	// Start HTTP server
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Set up debug logging for bubbletea
	f, err := tea.LogToFile(filepath.Join(filepath.Dir(*dbPath), "debug.log"), "claude-proxy")
	if err != nil {
		log.Fatalf("log to file: %v", err)
	}
	defer f.Close()

	// Run TUI
	model := tui.NewModel(st, *session)
	prog := tea.NewProgram(model)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}

	// Shutdown sequence
	// 1. Graceful HTTP shutdown (wait for in-flight requests)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}

	// 2. Close DB write channel and drain
	close(dbChan)
	<-dbDone

	// 3. Close database
	d.Close()
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude-proxy-data.db"
	}
	return filepath.Join(home, ".claude-proxy", "data.db")
}

// parseDuration extends time.ParseDuration to support the "d" (days) suffix.
// "7d" is converted to 7*24h before parsing.
func parseDuration(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid day value in %q: %w", s, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

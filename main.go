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
	flag.Parse()

	// Resolve --last to --from
	if *last != "" && *from == "" {
		dur, err := time.ParseDuration(*last)
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

		err = query.RunQuery(d, *queryMode, query.Opts{
			From:          *from,
			To:            *to,
			TTFTThreshold: *ttftThreshold,
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
				// Log to file, not stdout (TUI owns stdout)
			}
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
	f, err := tea.LogToFile("claude-proxy-debug.log", "claude-proxy")
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
	srv.Shutdown(ctx)

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

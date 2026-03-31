package query

import (
	"claude-proxy/db"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"
)

type Opts struct {
	From          string
	To            string
	SessionID     string
	TTFTThreshold int
	Writer        io.Writer
}

func RunQuery(d *db.DB, queryType string, opts Opts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	switch queryType {
	case "sessions":
		return runSessions(d, w)
	case "requests":
		return runRequests(d, w, opts)
	case "throttle":
		return runThrottle(d, w, opts)
	case "summary":
		return runSummary(d, w, opts)
	default:
		return fmt.Errorf("unknown query type: %s (valid: sessions, requests, throttle, summary)", queryType)
	}
}

func runSessions(d *db.DB, w io.Writer) error {
	sessions, err := d.QuerySessions()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tREQUESTS\tTOTAL TOKENS\tFIRST SEEN\tLAST SEEN")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n",
			s.ID, s.RequestCount, s.TotalTokens,
			s.FirstSeen.Format("2006-01-02 15:04:05"),
			s.LastSeen.Format("2006-01-02 15:04:05"))
	}
	return tw.Flush()
}

func runRequests(d *db.DB, w io.Writer, opts Opts) error {
	rows, err := d.QueryRequests(opts.From, opts.To, opts.SessionID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tMODEL\tSTATUS\tIN\tOUT\tCACHE\tTTFT\tLATENCY")
	for _, r := range rows {
		latency := r.EndTime.Sub(r.StartTime).Truncate(100 * time.Millisecond)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%dms\t%s\n",
			r.StartTime.Format("15:04:05"), r.SessionID, r.Model, r.StatusCode,
			r.InputTokens, r.OutputTokens, r.CacheRead,
			r.TTFT.Milliseconds(), latency)
	}
	return tw.Flush()
}

func runThrottle(d *db.DB, w io.Writer, opts Opts) error {
	threshold := opts.TTFTThreshold
	if threshold == 0 {
		threshold = 5000
	}
	rows, err := d.QueryThrottle(threshold, opts.From, opts.To, opts.SessionID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tMODEL\tSTATUS\tTTFT\tRETRY-AFTER\tREQ-REM\tTOK-REM\tERROR")
	for _, r := range rows {
		errStr := ""
		if r.HasError {
			errStr = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%dms\t%s\t%d\t%d\t%s\n",
			r.StartTime.Format("15:04:05"), r.SessionID, r.Model, r.StatusCode,
			r.TTFT.Milliseconds(), r.RetryAfter,
			r.RateLimitReqRemaining, r.RateLimitTokRemaining, errStr)
	}
	return tw.Flush()
}

func runSummary(d *db.DB, w io.Writer, opts Opts) error {
	summary, err := d.QuerySummary(opts.From, opts.To)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Total Requests:   %v\n", summary["total_requests"])
	fmt.Fprintf(w, "Total Tokens:     %v\n", summary["total_tokens"])
	fmt.Fprintf(w, "Sessions:         %v\n", summary["session_count"])
	fmt.Fprintf(w, "Avg TTFT:         %.0fms\n", summary["avg_ttft_ms"])
	fmt.Fprintf(w, "Throttle Events:  %v\n", summary["throttle_events"])
	return nil
}

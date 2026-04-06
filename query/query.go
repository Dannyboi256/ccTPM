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
	Limit         int
	Writer        io.Writer

	// TPM mode
	Bucket  int    // bucket size in seconds (default 300 = 5m)
	Peak    bool   // single-row peak mode
	GroupBy string // "" or "session"
}

func RunQuery(d *db.DB, queryType string, opts Opts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	switch queryType {
	case "sessions":
		return runSessions(d, w, opts.Limit)
	case "requests":
		return runRequests(d, w, opts)
	case "throttle":
		return runThrottle(d, w, opts)
	case "summary":
		return runSummary(d, w, opts)
	case "tpm":
		return runTPM(d, w, opts)
	default:
		return fmt.Errorf("unknown query type: %s (valid: sessions, requests, throttle, summary, tpm)", queryType)
	}
}

func runSessions(d *db.DB, w io.Writer, limit int) error {
	if limit <= 0 {
		limit = 10
	}
	sessions, err := d.QuerySessions(limit)
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
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	rows, err := d.QueryRequests(opts.From, opts.To, opts.SessionID, opts.Limit)
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
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	threshold := opts.TTFTThreshold
	if threshold == 0 {
		threshold = 5000
	}
	rows, err := d.QueryThrottle(threshold, opts.From, opts.To, opts.SessionID, opts.Limit)
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

func runTPM(d *db.DB, w io.Writer, opts Opts) error {
	from, to, err := resolveTPMRange(opts)
	if err != nil {
		return err
	}

	if opts.Peak {
		return runTPMPeak(d, w, from, to, opts)
	}
	return runTPMBuckets(d, w, from, to, opts)
}

func resolveTPMRange(opts Opts) (time.Time, time.Time, error) {
	to := time.Now()
	from := to.Add(-1 * time.Hour)
	if opts.From != "" {
		t, err := time.Parse("2006-01-02 15:04:05", opts.From)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --from: %w", err)
		}
		from = t
	}
	if opts.To != "" {
		t, err := time.Parse("2006-01-02 15:04:05", opts.To)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --to: %w", err)
		}
		to = t
	}
	return from, to, nil
}

func runTPMBuckets(d *db.DB, w io.Writer, from, to time.Time, opts Opts) error {
	bucket := opts.Bucket
	if bucket <= 0 {
		bucket = 300 // 5 minutes
	}
	buckets, err := d.QueryTPMBuckets(from, to, bucket)
	if err != nil {
		return err
	}
	if opts.Limit > 0 && len(buckets) > opts.Limit {
		buckets = buckets[:opts.Limit]
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET_START\tMAX_ITPM\tMAX_OTPM\tMAX_RPM\tSAMPLES")
	for _, b := range buckets {
		fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%d\t%d\n",
			b.BucketStart.Local().Format("2006-01-02 15:04"),
			b.MaxITPM, b.MaxOTPM, b.MaxRPM, b.SampleMinutes)
	}
	return tw.Flush()
}

func runTPMPeak(d *db.DB, w io.Writer, from, to time.Time, opts Opts) error {
	peaks, err := d.QueryTPMPeak(from, to, opts.GroupBy)
	if err != nil {
		return err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(peaks) > limit {
		peaks = peaks[:limit]
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if opts.GroupBy == "session" {
		fmt.Fprintln(tw, "SESSION\tMAX_ITPM\tMAX_OTPM\tMAX_RPM\tFIRST_SEEN\tLAST_SEEN")
		for _, p := range peaks {
			fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%d\t%s\t%s\n",
				p.SessionID, p.MaxITPM, p.MaxOTPM, p.MaxRPM,
				p.FirstSeen.Local().Format("2006-01-02 15:04"),
				p.LastSeen.Local().Format("2006-01-02 15:04"))
		}
	} else {
		fmt.Fprintln(tw, "METRIC\tVALUE\tFIRST_SEEN\tLAST_SEEN")
		if len(peaks) == 0 {
			return tw.Flush()
		}
		p := peaks[0]
		fmt.Fprintf(tw, "ITPM\t%.0f\t%s\t%s\n", p.MaxITPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(tw, "OTPM\t%.0f\t%s\t%s\n", p.MaxOTPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(tw, "RPM\t%d\t%s\t%s\n", p.MaxRPM,
			p.FirstSeen.Local().Format("2006-01-02 15:04"),
			p.LastSeen.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

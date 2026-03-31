package proxy

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestTeeReadCloserForwardsData(t *testing.T) {
	original := io.NopCloser(bytes.NewReader([]byte("hello world")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, time.Now())

	buf := make([]byte, 11)
	n, err := trc.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(buf[:n]))
	}

	trc.Close()

	// Verify data was sent to channel
	var received []byte
	for chunk := range ch {
		received = append(received, chunk...)
	}
	if string(received) != "hello world" {
		t.Fatalf("channel received '%s', expected 'hello world'", string(received))
	}
}

func TestTeeReadCloserTTFT(t *testing.T) {
	start := time.Now()
	original := io.NopCloser(bytes.NewReader([]byte("data")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, start)

	time.Sleep(10 * time.Millisecond)
	buf := make([]byte, 4)
	trc.Read(buf)

	ttft := trc.TTFT()
	if ttft < 10*time.Millisecond {
		t.Fatalf("TTFT too short: %v", ttft)
	}

	trc.Close()
}

func TestTeeReadCloserCloseSignalsChannel(t *testing.T) {
	original := io.NopCloser(bytes.NewReader([]byte("x")))
	ch := make(chan []byte, 64)
	trc := newTeeReadCloser(original, ch, time.Now())

	buf := make([]byte, 1)
	trc.Read(buf)
	trc.Close()

	select {
	case _, ok := <-ch:
		if ok {
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after Close()")
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		fallback string
		want     string
	}{
		{"x-session-id", http.Header{"X-Session-Id": {"abc"}}, "default", "abc"},
		{"anthropic-session-id", http.Header{"Anthropic-Session-Id": {"def"}}, "default", "def"},
		{"fallback", http.Header{}, "my-session", "my-session"},
		{"x-session-id takes priority", http.Header{
			"X-Session-Id":         {"first"},
			"Anthropic-Session-Id": {"second"},
		}, "default", "first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSessionID(tt.headers, tt.fallback)
			if got != tt.want {
				t.Fatalf("expected '%s', got '%s'", tt.want, got)
			}
		})
	}
}

package scan_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/scan"
	"github.com/tonym128/agent-cost-dashboard/internal/source"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
	"github.com/tonym128/agent-cost-dashboard/internal/web"
)

// TestScannerWriterHTTPReaderCoexist proves the package's central claim: a page
// served while the scanner is committing is one consistent database. Nothing
// else in the suite exercises a writer and a web read against the same store at
// the same time.
func TestScannerWriterHTTPReaderCoexist(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent smoke test")
	}
	home := t.TempDir()
	logPath := filepath.Join(home, ".pi", "agent", "sessions", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"type":"session","id":"s1","cwd":"/tmp/p","timestamp":"2026-05-06T09:00:00.000Z"}`)
	write(`{"type":"message","timestamp":"2026-05-06T09:00:01.000Z","id":"m1","message":{"role":"assistant","model":"claude-sonnet-4-6","usage":{"input":10,"output":5}}}`)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pricer, err := source.NewPricer("", "")
	if err != nil {
		t.Fatal(err)
	}
	srcs, _ := scan.DefaultSources(home)
	sc := scan.New(scan.Config{
		Store:   st,
		Pricer:  pricer,
		Logger:  nil,
		Sources: srcs,
	})

	srv, err := web.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx, 5*time.Millisecond)

	// Keep appending new records so every scanner pass has something to write.
	stopFeed := make(chan struct{})
	go func() {
		i := 2
		for {
			select {
			case <-stopFeed:
				return
			default:
				write(fmt.Sprintf(`{"type":"message","timestamp":"2026-05-06T09:00:05.000Z","id":"m%d","message":{"role":"assistant","model":"claude-sonnet-4-6","usage":{"input":10,"output":5}}}`, i))
				i++
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer close(stopFeed)

	var failures atomic.Int64
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			failures.Add(1)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			failures.Add(1)
		}
		resp2, err := http.Get(ts.URL + "/api/activity?window=all")
		if err == nil {
			var v any
			_ = json.NewDecoder(resp2.Body).Decode(&v)
			resp2.Body.Close()
		}
	}
	if failures.Load() > 0 {
		t.Fatalf("%d page loads failed while scanning", failures.Load())
	}
}

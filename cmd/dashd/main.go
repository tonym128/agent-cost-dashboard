// Command dashd serves the cost dashboard and keeps its database up to date.
//
// The two halves are deliberately separate. The scanner is a background loop that
// owns writing; the web server only ever reads. SQLite runs in WAL mode, so a
// page load gets a consistent snapshot while a scan is in progress and neither
// half waits on the other. Either can also be run alone — `dashd scan` for a
// cron-driven machine, `dashd serve` when the data is maintained elsewhere.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tonym/agent-cost-dashboard/internal/scan"
	"github.com/tonym/agent-cost-dashboard/internal/source"
	"github.com/tonym/agent-cost-dashboard/internal/store"
	"github.com/tonym/agent-cost-dashboard/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dashd:", err)
		os.Exit(1)
	}
}

type options struct {
	db       string
	addr     string
	interval time.Duration
	home     string
	models   string
	opencode string
	verbose  bool
}

func run() error {
	fs := flag.NewFlagSet("dashd", flag.ContinueOnError)
	command := "serve-and-scan"
	if len(os.Args) > 1 && len(os.Args[1]) > 0 && os.Args[1][0] != '-' {
		command = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	home, _ := os.UserHomeDir()
	var o options
	fs.StringVar(&o.db, "db", filepath.Join(home, ".local/share/dashd/dashboard.db"),
		"path to the SQLite database")
	fs.StringVar(&o.addr, "addr", "127.0.0.1:8753",
		"address to serve on; 0.0.0.0 exposes the dashboard to the network")
	fs.DurationVar(&o.interval, "interval", 30*time.Second,
		"how often to scan for new activity")
	fs.StringVar(&o.home, "home", home, "directory to look for agent logs under")
	fs.StringVar(&o.models, "models", defaultModelsPath(),
		"path to the OpenRouter models.json price dump")
	fs.StringVar(&o.opencode, "opencode", "",
		"path to the OpenCode database (default: inside -home)")
	fs.BoolVar(&o.verbose, "verbose", false, "log every request and scan detail")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	switch command {
	case "serve-and-scan", "serve", "scan", "reprice", "stats":
	default:
		return fmt.Errorf("unknown command %q (want serve-and-scan, serve, scan, reprice or stats)", command)
	}

	if o.opencode == "" {
		o.opencode = filepath.Join(o.home, ".local/share/opencode/opencode.db")
	}

	st, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer st.Close()

	pricer, err := source.NewPricer(o.models, "")
	if err != nil {
		return fmt.Errorf("%w\n\nPricing is required. Fetch it with:\n  "+
			"python3 update_models.py", err)
	}

	sources, openCodeDB := scan.DefaultSources(o.home)
	scanner := scan.New(scan.Config{
		Store:      st,
		Pricer:     pricer,
		Logger:     log,
		Sources:    sources,
		OpenCodeDB: openCodeDB,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "scan":
		// One pass and out, for a machine that would rather be driven by cron
		// or a systemd timer than by a resident process.
		if err := scanner.RunOnce(ctx); err != nil {
			return err
		}
		return printStats(st, log)

	case "reprice":
		n, err := st.Reprice(ctx, func(m string, in, out, cacheRead, cacheWrite int64) (float64, bool) {
			return pricer.Cost(m, int(in), int(out), int(cacheRead), int(cacheWrite))
		})
		if err != nil {
			return err
		}
		log.Info("repriced", "calls", n, "prices", o.models)
		return printStats(st, log)

	case "stats":
		return printStats(st, log)
	}

	srv, err := web.New(st, log)
	if err != nil {
		return err
	}
	srv.ScanInfo = scanner.Status

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if command == "serve-and-scan" {
		go scanner.Run(ctx, o.interval)
	}

	httpSrv := &http.Server{
		Addr:    o.addr,
		Handler: srv.Handler(),
		// Generous, because a first render over a large history is genuinely
		// slower than a static page but not unbounded.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.addr, err)
	}
	log.Info("serving",
		"url", "http://"+ln.Addr().String(),
		"db", o.db,
		"scanning", command == "serve-and-scan",
		"interval", o.interval,
		"pid", os.Getpid())
	log.Info("press ctrl-c to stop; -verbose for per-request detail")

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Stop accepting requests, let in-flight ones finish, then close the
	// database. Closing it first would fail whatever a request was still using.
	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
	}
	return nil
}

func printStats(st *store.Store, log *slog.Logger) error {
	ctx := context.Background()
	totals, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		return err
	}
	statuses, _ := st.ScanStatuses()
	fmt.Printf("calls      %d\n", totals.Calls)
	fmt.Printf("sessions   %d\n", totals.Sessions)
	fmt.Printf("projects   %d\n", totals.Projects)
	fmt.Printf("tokens     %d\n", totals.TotalTokens)
	fmt.Printf("cost       $%.2f\n", totals.Cost)
	if totals.UnpricedCalls > 0 {
		fmt.Printf("unpriced   %d calls (no rate found; run `update_models.py`)\n", totals.UnpricedCalls)
	}
	if totals.FirstTS > 0 {
		oldest, newest, _ := st.HistoryRange(ctx, store.Filter{})
		fmt.Printf("history    %s to %s\n",
			oldest.Local().Format("2006-01-02"), newest.Local().Format("2006-01-02"))
	}
	fmt.Println()
	for _, s := range statuses {
		state := "ok"
		if s.Error != "" {
			state = s.Error
		}
		fmt.Printf("%-9s %6d files  %6d changed  %6d calls  %s\n",
			s.Agent, s.FilesSeen, s.FilesChanged, s.CallsIngested, state)
	}
	return nil
}

// defaultModelsPath looks for the price dump next to the repository, which is
// where it lives in a checkout, and under the data directory otherwise.
func defaultModelsPath() string {
	if exe, err := os.Executable(); err == nil {
		// The binary sits beside models.json in a checkout, and one level above
		// it when installed into ~/bin or /usr/local/bin.
		dir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(dir, "models.json"),
			filepath.Join(filepath.Dir(dir), "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		for _, candidate := range []string{
			filepath.Join(wd, "models.json"),
			filepath.Join(wd, "..", "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "models.json"
}

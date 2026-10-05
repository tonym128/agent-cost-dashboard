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
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/scan"
	"github.com/tonym128/agent-cost-dashboard/internal/source"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
	"github.com/tonym128/agent-cost-dashboard/internal/web"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

// runCLI parses the command line, handles the two options that print and exit,
// and otherwise runs the command. Kept separate from main so the exit code and
// the streams are testable without starting a server.
func runCLI(argv []string, stdout, stderr io.Writer) int {
	o, command, err := parseArgs(argv)
	switch {
	case errors.Is(err, flag.ErrHelp):
		// `-h` is a request, not a failure.
		printUsage(stderr)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "dashd: %v\n\n", err)
		printUsage(stderr)
		return 2
	}

	if o.showVer {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	if err := run(o, command, stdout); err != nil {
		fmt.Fprintln(stderr, "dashd:", err)
		return 1
	}
	return 0
}

func run(o options, command string, stdout io.Writer) error {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting", "version", version, "commit", commit, "date", date, "command", command)
	if o.opencode == "" {
		o.opencode = filepath.Join(o.home, ".local/share/opencode/opencode.db")
	}

	st, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer st.Close()

	// The pricer is built before the command switch on purpose: every command
	// that costs anything needs it, so this is the single place the fallback
	// warning can be — once per process, for scan, reprice, stats and serve.
	pricer, err := source.NewPricer(o.models, "")
	if err != nil {
		return fmt.Errorf("%w\n\nPricing is required. Fetch it with:\n  "+
			"python3 update_models.py", err)
	}
	warnIfFallbackOnly(pricer, o.models, log)

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
		return printStats(st, stdout)

	case "reprice":
		n, err := st.Reprice(ctx, func(m string, in, out, cacheRead, cacheWrite int64) (float64, bool) {
			return pricer.Cost(m, int(in), int(out), int(cacheRead), int(cacheWrite))
		})
		if err != nil {
			return err
		}
		log.Info("repriced", "calls", n, "prices", o.models)
		return printStats(st, stdout)

	case "stats":
		return printStats(st, stdout)
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

	logExposure(o, log)

	httpSrv := &http.Server{
		Addr: o.addr,
		// Authentication is a wrapper rather than something the dashboard knows
		// about, so this file decides policy and web/ stays a dashboard.
		Handler: web.WithAuth(o.authToken, log, srv.Handler()),
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
		"version", version,
		"db", o.db,
		"scanning", command == "serve-and-scan",
		"auth", o.authToken != "",
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
	// The scanner is stopped by the deferred wait above, after Shutdown returns
	// and before the store closes.
	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
	}
	return nil
}

// logExposure says plainly what the bind address implies. `dashd -addr 0.0.0.0`
// on a cloud VM is a documented, and reasonable, thing for someone to do — which
// is exactly why the unauthenticated case needs to be loud at startup rather
// than discovered later.
func logExposure(o options, log *slog.Logger) {
	exposed := exposedToNetwork(o.addr)
	switch {
	case !exposed && o.authToken == "":
		// Nothing to say. Localhost, no token: the normal case, and a warning
		// here would train the operator to ignore warnings.
	case exposed && o.authToken == "":
		log.Warn("serving on a non-loopback address with NO authentication",
			"addr", o.addr,
			"exposes", "project paths, session titles, model names and per-session cost to anyone who can reach this port",
			"reachable_from", "every interface on this host, including its public one",
			"fix", "set -auth-token, or bind to 127.0.0.1 and reach it over an SSH tunnel")
	case exposed && o.authToken != "":
		log.Info("serving on a non-loopback address, protected by -auth-token",
			"addr", o.addr, "accepts", "Authorization: Bearer, or X-Auth-Token",
			"exempt", "/healthz")
	default:
		log.Info("authentication enabled", "accepts", "Authorization: Bearer, or X-Auth-Token",
			"exempt", "/healthz")
	}
}

// exposedToNetwork reports whether addr binds somewhere other than this machine.
func exposedToNetwork(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Unparseable is not a reason to be reassuring.
		return true
	}
	switch strings.ToLower(host) {
	case "":
		// ":8753" and "0.0.0.0:8753" both mean every interface.
		return true
	case "localhost":
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// warnIfFallbackOnly reports the one degraded-but-working state: the OpenRouter
// price dump did not load and every price is coming from the embedded table.
//
// This is not fatal, and deliberately so. The fallback table exists so that a
// binary without a dump beside it still runs, and refusing to start would turn a
// rough number into no number. It is also the reason this is loud: with the dump
// absent, a model the fallback knows is priced correctly, but a model it does
// not know is *unpriced* — and a stale row is worse than a missing one, because
// a missing one is at least visible in the unpriced count. Silence here is how a
// `$0.00` with `priced=1` reaches the dashboard.
func warnIfFallbackOnly(pricer *source.Pricer, modelsPath string, log *slog.Logger) {
	if !pricer.FallbackOnly() {
		return
	}
	log.Warn("OpenRouter price dump not loaded; pricing accuracy is degraded",
		"reason", "price dump missing or unreadable",
		"models", modelsPath,
		"priced_from", "embedded fallback table",
		"fallback_models", pricer.FallbackSize(),
		"live_models", 0,
		"effect", "models absent from the fallback table are reported unpriced; "+
			"fallback rows may be older than the provider's current rates",
		"fix", "run `python3 update_models.py` in the dashd checkout, or pass -models /path/to/models.json",
	)
}

// printStats reports the totals, including how many calls could not be priced at
// all — the number that says the rate table is missing something.
func printStats(st *store.Store, out io.Writer) error {
	ctx := context.Background()
	totals, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		return err
	}
	statuses, _ := st.ScanStatuses()
	fmt.Fprintf(out, "calls      %d\n", totals.Calls)
	fmt.Fprintf(out, "sessions   %d\n", totals.Sessions)
	fmt.Fprintf(out, "projects   %d\n", totals.Projects)
	fmt.Fprintf(out, "tokens     %d\n", totals.TotalTokens)
	fmt.Fprintf(out, "cost       $%.2f\n", totals.Cost)
	if totals.UnpricedCalls > 0 {
		fmt.Fprintf(out, "unpriced   %d calls (no rate found; run `update_models.py`)\n", totals.UnpricedCalls)
	}
	if totals.FirstTS > 0 {
		oldest, newest, _ := st.HistoryRange(ctx, store.Filter{})
		fmt.Fprintf(out, "history    %s to %s\n",
			oldest.Local().Format("2006-01-02"), newest.Local().Format("2006-01-02"))
	}
	fmt.Fprintln(out)
	for _, s := range statuses {
		state := "ok"
		if s.Error != "" {
			state = s.Error
		}
		fmt.Fprintf(out, "%-9s %6d files  %6d changed  %6d calls  %s\n",
			s.Agent, s.FilesSeen, s.FilesChanged, s.CallsIngested, state)
	}
	return nil
}

// defaultModelsPath looks for the price dump next to the repository, which is
// where it lives in a checkout, and under the data directory otherwise.
func defaultModelsPath() string {
	exe, _ := os.Executable()
	wd, _ := os.Getwd()
	return modelsPathFrom(exe, wd)
}

// modelsPathFrom is defaultModelsPath with the two environment lookups handed in,
// so the search order can be tested against a real filesystem instead of
// whatever directory the test binary happens to sit in.
func modelsPathFrom(exePath, workDir string) string {
	if exePath != "" {
		// The binary sits beside models.json in a checkout, and one level above
		// it when installed into ~/bin or /usr/local/bin.
		dir := filepath.Dir(exePath)
		for _, candidate := range []string{
			filepath.Join(dir, "models.json"),
			filepath.Join(filepath.Dir(dir), "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if workDir != "" {
		for _, candidate := range []string{
			filepath.Join(workDir, "models.json"),
			filepath.Join(workDir, "..", "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "models.json"
}

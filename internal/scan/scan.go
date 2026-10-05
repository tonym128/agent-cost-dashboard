// Package scan discovers the source files, drives the parsers over them, and
// keeps the store current.
//
// The scanner is incremental by default: a pass stats every known file, skips
// the ones whose size and modification time are unchanged, and for the rest
// reads only what lies beyond the previous pass's cursor. That is what makes a
// periodic background pass cheap enough to run every few seconds, which is in
// turn what makes the dashboard feel live rather than cached.
package scan

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"github.com/tonym128/agent-cost-dashboard/internal/source"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

// Source describes where one agent keeps its session logs.
type Source struct {
	Agent string
	// Root is the directory tree searched.
	Root string
	// Ext is the file extension to match, including the dot.
	Ext string
	// Recurse walks subdirectories. Set for agents that shard by project.
	Recurse bool
	// SkipDirs are directory names never worth descending into.
	SkipDirs []string

	// parserFor produces the parser for one discovered unit of work.
	parserFor func() source.Parser
}

// Config configures a Scanner.
type Config struct {
	Store   *store.Store
	Pricer  *source.Pricer
	Logger  *slog.Logger
	Sources []Source

	// OpenCodeDB, when set, adds OpenCode as a source. It is handled separately
	// because every session lives in one shared database: the unit of work is a
	// row, not a file.
	OpenCodeDB string

	// Now is injectable for tests.
	Now func() time.Time
}

// Scanner performs scan passes over a set of sources.
type Scanner struct {
	cfg Config
	log *slog.Logger

	// mu serialises passes. Two concurrent passes would race on the same cursor
	// and could double-append a file that grew between the two stats.
	mu sync.Mutex

	// lastRun is when a pass last completed, exposed for the status line.
	lastRun time.Time
	running bool
}

// New builds a Scanner.
func New(cfg Config) *Scanner {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Scanner{cfg: cfg, log: cfg.Logger}
}

// RunOnce performs one full pass and returns what it did.
func (s *Scanner) RunOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = true
	defer func() { s.running = false }()

	start := s.cfg.Now()
	s.log.Debug("scan starting",
		"sources", len(s.cfg.Sources), "opencode", s.cfg.OpenCodeDB != "")

	states, err := s.cfg.Store.LoadScanStates()
	if err != nil {
		return fmt.Errorf("load scan state: %w", err)
	}

	changedFiles, changedSessions := 0, 0
	for _, src := range s.cfg.Sources {
		found, ingested, err := s.scanSource(ctx, src, states)
		if err != nil {
			// One broken source must not stop the others; the failure is
			// recorded on that source's status and surfaced in the UI.
			s.log.Error("source scan failed", "agent", src.Agent, "error", err)
			s.recordStatus(src.Agent, found.seen, found.changed, 0, err)
			continue
		}
		changedFiles += found.changed
		changedSessions += ingested
		s.recordStatus(src.Agent, found.seen, found.changed, ingested, nil)
	}

	if s.cfg.OpenCodeDB != "" {
		seen, changed, ingested, err := s.scanOpenCode(ctx, states)
		if err != nil {
			s.log.Error("opencode scan failed", "error", err)
			s.recordStatus(model.AgentOpencode, seen, changed, ingested, err)
		} else {
			s.recordStatus(model.AgentOpencode, seen, changed, ingested, nil)
		}
	}

	// Flag sessions whose source file has vanished. The history is kept and only
	// the transcript is withdrawn; see markVanished.
	if vanished, err := s.markVanished(states); err != nil {
		s.log.Warn("could not flag vanished sources", "error", err)
	} else if vanished > 0 {
		s.log.Info("source logs no longer present; history kept, transcripts withdrawn",
			"files", vanished)
	}

	s.lastRun = s.cfg.Now()
	// Quiet when idle. On a short interval a log line per pass buries anything
	// that matters, and "nothing changed" is the expected case rather than an
	// event worth reporting.
	if changedFiles > 0 || changedSessions > 0 {
		s.log.Info("scan ingested new activity",
			"duration", s.lastRun.Sub(start).Round(time.Millisecond),
			"files_changed", changedFiles,
			"calls_ingested", changedSessions)
	} else {
		s.log.Debug("scan complete",
			"duration", s.lastRun.Sub(start).Round(time.Millisecond))
	}
	return nil
}

// Run repeatedly until the context is cancelled. This is the background half of
// the process: it never returns on its own, and it holds no reference to the web
// layer, so a slow pass cannot delay a page load.
func (s *Scanner) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	// A first pass immediately, so the database is populated before the first
	// request rather than after the first tick.
	s.runOnceLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("scanner stopping")
			return
		case <-ticker.C:
			s.runOnceLogged(ctx)
		}
	}
}

func (s *Scanner) runOnceLogged(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("scan pass failed", "error", err)
	}
}

// Status reports what the scanner is doing, for the status endpoint.
func (s *Scanner) Status() (lastRun time.Time, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun, s.running
}

// ---------------------------------------------------------------- one source

type fileTally struct{ seen, changed int }

func (s *Scanner) scanSource(ctx context.Context, src Source, states map[string]model.ScanState) (fileTally, int, error) {
	paths, err := discover(src)
	if err != nil {
		return fileTally{}, 0, err
	}
	tally := fileTally{seen: len(paths)}
	ingested := 0

	// A cap on how many files one pass will fully re-read. A machine that has
	// just come back from months of being off, or a source whose layout changed,
	// can present thousands of changed files at once; draining that in one pass
	// would hold the write lock long enough for the web layer to queue behind it.
	// The remainder is picked up on the next pass, which is why the status line
	// reports "changed" separately from "ingested".
	const maxChangedPerPass = 200

	for _, path := range paths {
		if ctx.Err() != nil {
			return tally, ingested, ctx.Err()
		}
		st := states[path]
		if st.Complete {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		// The cheap gate: an unchanged size and mtime means an unchanged file.
		// This is what makes a steady-state pass cost one stat per file.
		if st.Offset > 0 && info.Size() == st.Size && info.ModTime().Equal(st.ModTime) {
			continue
		}
		if tally.changed >= maxChangedPerPass {
			continue
		}
		tally.changed++

		n, err := s.ingest(path, st, src.parserFor())
		if err != nil {
			// A file that cannot be parsed is skipped and retried next pass
			// rather than being marked done, so a transient read error (a file
			// being rewritten underneath us) is not made permanent.
			s.log.Warn("skipping unreadable source", "path", path, "error", err)
			continue
		}
		ingested += n
	}
	return tally, ingested, nil
}

// ingest parses one file and writes the result.
//
// The append-versus-replace decision is made here, from evidence rather than
// assumption: the previous pass tells us how far it read and what the head of
// the file looked like, and only if the file still starts the same way and is
// no shorter may we treat the new bytes as an addition.
func (s *Scanner) ingest(path string, prev model.ScanState, parser source.Parser) (int, error) {
	sess, next, err := parser.Parse(path, prev, s.cfg.Pricer)
	if err != nil {
		return 0, err
	}
	if sess.UID == "" {
		return 0, nil
	}

	// A pass that read nothing new leaves the file byte-identical over the range
	// it has already consumed. Returning early matters: going on to replace the
	// session would delete rows the fresh parse never re-read, because an
	// incremental read returns only the increment. An idle pass would then
	// quietly erase the session it was supposed to leave alone.
	if prev.Offset > 0 && next.Offset == prev.Offset && sameHead(path, prev) {
		// Still refresh the bookkeeping so the recorded size and mtime stay
		// current; otherwise every future pass re-stats this file in full.
		next.Scanned = s.cfg.Now()
		if err := s.cfg.Store.SaveScanState(next); err != nil {
			return 0, fmt.Errorf("save state %s: %w", path, err)
		}
		return 0, nil
	}

	incremental := next.Offset > prev.Offset && prev.Offset > 0 && sameHead(path, prev)
	if incremental {
		err = s.cfg.Store.AppendSession(sess)
	} else {
		err = s.cfg.Store.ReplaceSession(sess)
	}
	if err != nil {
		return 0, fmt.Errorf("store %s: %w", path, err)
	}
	// The summary is rebuilt from the rows either way, which is what keeps an
	// append from leaving a stale total behind.
	if err := s.cfg.Store.RecomputeSession(sess.UID); err != nil {
		return 0, fmt.Errorf("summarise %s: %w", sess.UID, err)
	}

	next.Scanned = s.cfg.Now()
	if err := s.cfg.Store.SaveScanState(next); err != nil {
		return 0, fmt.Errorf("save state %s: %w", path, err)
	}
	// The log is present again, so whatever had been withdrawn comes back.
	if err := s.cfg.Store.ClearOrphaned(sess.UID); err != nil {
		return 0, fmt.Errorf("clear orphan flag %s: %w", sess.UID, err)
	}
	return len(sess.Calls), nil
}

// sameHead reports whether a file still begins with what the previous pass saw,
// which is the evidence that it was appended to rather than rewritten.
func sameHead(path string, prev model.ScanState) bool {
	return source.HeadMatches(path, prev.PrefixHash, prev.PrefixLen)
}

// ---------------------------------------------------------------- OpenCode

// scanOpenCode ingests every top-level session in the shared database.
//
// Returns (sessions seen, sessions changed, calls ingested). Unlike the file
// sources the unit here is a session rather than a file, so the counts describe
// sessions and are labelled as such on the status page.
func (s *Scanner) scanOpenCode(ctx context.Context, states map[string]model.ScanState) (seen, changed, ingested int, err error) {
	if _, err := os.Stat(s.cfg.OpenCodeDB); err != nil {
		return 0, 0, 0, nil // not installed; not an error
	}
	ids, err := openCodeSessionIDs(s.cfg.OpenCodeDB)
	if err != nil {
		return 0, 0, 0, err
	}
	seen = len(ids)

	// Keyed by the same "opencode:<id>" the state rows use.
	prev := make(map[string]model.ScanState, len(ids))
	for _, id := range ids {
		if st, ok := states["opencode:"+id]; ok {
			prev[id] = st
		}
	}

	// The concrete type, not the interface: batching is OpenCode-specific.
	parser := source.NewOpenCodeParser(s.cfg.OpenCodeDB)
	sessions, next, err := parser.ParseAll(ids, prev, s.cfg.Pricer)
	if err != nil {
		return seen, changed, ingested, err
	}

	for _, id := range ids {
		if ctx.Err() != nil {
			return seen, changed, ingested, ctx.Err()
		}
		state := next[id]
		sess := sessions[id]
		if sess.UID == "" {
			continue
		}
		// The cursor is a maximum update time, so a session is either unchanged
		// or has new rows; there is no partial state to reconcile and nothing to
		// gain from rewriting a session that has not moved.
		if p := prev[id]; p.Offset > 0 && state.Offset == p.Offset {
			continue
		}
		changed++
		// The parse above resumed past the stored cursor and returned only the
		// new rows, so a replacement commit would delete every call already
		// stored for this session. Only a first read — the one at a zero
		// cursor, which saw the whole session — may be committed as a
		// replacement. This is the same append-versus-replace rule ingest()
		// applies to the file sources, from the same evidence.
		commit := s.cfg.Store.AppendSession
		if prev[id].Offset == 0 {
			commit = s.cfg.Store.ReplaceSession
		}
		if err := commit(sess); err != nil {
			return seen, changed, ingested, err
		}
		if err := s.cfg.Store.RecomputeSession(sess.UID); err != nil {
			return seen, changed, ingested, err
		}
		state.Scanned = s.cfg.Now()
		if err := s.cfg.Store.SaveScanState(state); err != nil {
			return seen, changed, ingested, err
		}
		ingested += len(sess.Calls)
	}
	return seen, changed, ingested, nil
}

func openCodeSessionIDs(dbPath string) ([]string, error) {
	db, err := source.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT id FROM session WHERE parent_id IS NULL OR parent_id = '' ORDER BY time_created")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- discovery

// discover lists the source files for one agent.
func discover(src Source) ([]string, error) {
	if src.Root == "" {
		return nil, nil
	}
	if _, err := os.Stat(src.Root); err != nil {
		return nil, nil // not installed; not an error
	}
	skip := map[string]bool{}
	for _, d := range src.SkipDirs {
		skip[d] = true
	}

	var out []string
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is skipped, not fatal
		}
		if d.IsDir() {
			// The root's own name is exempt: these sources are rooted at
			// dot-directories like ~/.gemini, and applying the hidden-directory
			// rule to the root skips the entire tree.
			if path == src.Root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || skip[name] {
				return filepath.SkipDir
			}
			if !src.Recurse {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), src.Ext) {
			out = append(out, path)
		}
		return nil
	}
	if err := filepath.WalkDir(src.Root, walk); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// markVanished flags sessions whose source file has disappeared.
//
// The calls stay. A session log that has been rotated or deleted is exactly the
// case the individual call rows exist for: deleting the summary with it would
// mean that asking "what did I spend in March" stops working the moment March's
// logs are cleaned up. What is withdrawn is the transcript, and the flag is what
// tells the page not to offer it.
//
// The scan_state rows do go, so a dead path is not re-stat-ed on every pass.
func (s *Scanner) markVanished(states map[string]model.ScanState) (int, error) {
	var gone []string
	var uids []string
	for path, st := range states {
		if st.Path == "" {
			continue
		}
		if strings.HasPrefix(st.Path, "opencode:") {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			gone = append(gone, path)
			if st.SessionUID != "" {
				uids = append(uids, st.SessionUID)
			}
		}
	}
	if len(gone) == 0 {
		return 0, nil
	}
	if err := s.cfg.Store.ForgetScanStates(gone); err != nil {
		return 0, err
	}
	if err := s.cfg.Store.MarkOrphaned(uids); err != nil {
		return 0, err
	}
	return len(gone), nil
}

func (s *Scanner) recordStatus(agent string, seen, changed, ingested int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	st := model.ScanStatus{
		Agent:         agent,
		LastScanAt:    s.cfg.Now(),
		FilesSeen:     seen,
		FilesChanged:  changed,
		CallsIngested: ingested,
		Error:         msg,
		LastFull:      changed > 0,
	}
	if changed > 0 {
		st.LastFullAt = s.cfg.Now()
	}
	if err := s.cfg.Store.RecordScanStatus(agent, st); err != nil {
		s.log.Warn("could not record scan status", "agent", agent, "error", err)
	}
}

// defaultSkipDirs are directories that never contain session logs but can hold
// thousands of files.
var defaultSkipDirs = []string{"node_modules", ".git", "__pycache__", "target", "dist", "build"}

// HomeRelativeRoots is where each agent keeps its session logs, as a path
// relative to the user's home directory.
//
// It is exported because the same six locations are needed by anything that has
// to tell a reader where to look: the web layer's source panel shows a path for
// every agent so a miss can be acted on, and a hint pointing somewhere the
// scanner never looked is worse than no hint at all. Keeping the list here means
// the hint and the scan cannot disagree — which is the defect this replaced, where
// the two lists were duplicated and nothing checked they agreed.
//
// OpenCode is a file rather than a tree, so it is a path to a database; the
// scanner takes it separately from the tree-shaped sources.
var HomeRelativeRoots = map[string]string{
	model.AgentPi:       ".pi/agent/sessions",
	model.AgentClaude:   ".claude/projects",
	model.AgentCodex:    ".codex/sessions",
	model.AgentGemini:   ".gemini",
	model.AgentAgy:      ".gemini/antigravity/conversations",
	model.AgentOpencode: ".local/share/opencode/opencode.db",
}

// HomeRelativeRoot returns one agent's log location relative to the home
// directory, and whether this build knows the agent at all.
func HomeRelativeRoot(agent string) (string, bool) {
	rel, ok := HomeRelativeRoots[agent]
	return rel, ok
}

// DefaultSources returns the agents this project knows how to read, located
// under the given home directory.
//
// The roots come from HomeRelativeRoots rather than being spelled out here, so
// the location shown on the page and the location walked are the same value.
func DefaultSources(home string) ([]Source, string) {
	root := func(agent string) string { return filepath.Join(home, HomeRelativeRoots[agent]) }
	sources := []Source{
		{
			Agent: model.AgentPi, Root: root(model.AgentPi),
			Ext: ".jsonl", Recurse: true, SkipDirs: defaultSkipDirs,
			parserFor: func() source.Parser { return source.NewPiParser() },
		},
		{
			Agent: model.AgentClaude, Root: root(model.AgentClaude),
			Ext: ".jsonl", Recurse: true, SkipDirs: defaultSkipDirs,
			parserFor: func() source.Parser { return source.NewClaudeParser() },
		},
		{
			Agent: model.AgentCodex, Root: root(model.AgentCodex),
			Ext: ".jsonl", Recurse: true, SkipDirs: defaultSkipDirs,
			parserFor: func() source.Parser { return source.NewCodexParser() },
		},
		{
			Agent: model.AgentGemini, Root: root(model.AgentGemini),
			Ext: ".jsonl", Recurse: true, SkipDirs: defaultSkipDirs,
			parserFor: func() source.Parser { return source.NewGeminiParser() },
		},
		{
			Agent: model.AgentAgy, Root: root(model.AgentAgy),
			Ext: ".db", Recurse: false, SkipDirs: defaultSkipDirs,
			parserFor: func() source.Parser { return source.NewAntigravityParser() },
		},
	}
	return sources, root(model.AgentOpencode)
}

// testLogger is referenced by the package tests; declared here so the tests do
// not have to import slog.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **A model-filtered page reported `0` tool seconds.** `toolSeconds` built its
  clause with the call-table helper, emitting `model IN (...)` against
  `tool_call` — a table with no `model` column — and discarded the resulting SQL
  error into a confident `0`. The "Tool Time" card read `0s`, and because every
  tool row's percentage divides by it, the whole tool table showed `0%` shares
  against non-zero seconds on the same screen. This is the same defect class as
  the `?model=` 500 fixed below, in the one call site that was missed; it now
  uses the tool-table helper and returns the error instead of swallowing it.
- **The session page double-escaped untrusted text.** `sessionView` and
  `callViews` pre-escaped with `html.EscapeString` on top of `html/template`'s
  own escaping, so a session title, project path or model name containing
  `& < > " '` rendered as literal `&amp;amp;` on `/session`. The index page was
  unaffected, which is why the escaping test did not catch it: it only requested
  `/`.
- **An incremental append could move a session out of its project bucket.** An
  append resumes past the log's header record, which is where most agents carry
  the working directory, so the parser reports no project. That empty value was
  written over the stored one, so a growing pi, Codex or Gemini session drifted
  into an empty project and split the per-project rollup across two rows. The
  stored project is now recovered before writing, and an empty per-row project
  falls back to it.
- **An Antigravity step with no start timestamp was dated 1970-01-01.** The
  timestamp was computed before the guard that checked for one, so a step
  missing that field carried a real cost into a 1970 day bucket, which rendered
  as a date row and a "January 1970" monthly total. Zero is the zero time here,
  as it already was for every other parser.
- **A date filter mis-bounded its end day across a DST transition.** The upper
  bound was computed as midnight plus an absolute 24 hours less a second, which
  over-included the first hour of the next day on the spring-forward and
  silently dropped the last hour of the named day on the fall-back. It is
  calendar arithmetic now, and the test pins both transitions.
- **`/healthz` could not stall behind a scan, and neither could a page load see
  a torn database.** Two related fixes to the claim the package makes about WAL:
  `Scanner.Status` took the same mutex a whole pass holds, so the health probe
  blocked for the pass's duration; and the index handler issued eleven separate
  queries with no transaction, so its payload could describe two different
  databases when a scan committed mid-render. `Status` now reads the pass state
  under its own lock, and the page's rollups all run inside one read
  transaction, which WAL serves without blocking the writer.
- **The orphaned-session state never reached the page.** The mechanism worked —
  a session whose log has been rotated away is flagged, its figures kept — but
  neither the sessions payload nor the session view emitted the flag, so every
  row offered a transcript link that cannot open and the explanation never
  rendered. The documented behaviour is what the page now does.
- **`dashd scan` exited 0 when every source failed.** Per-source errors were
  recorded for the UI and then discarded, so a cron job or a systemd
  `Type=oneshot` timer reported success after ingesting nothing — invisible to
  exactly the supervisors the split `scan`/`serve` deployment depends on. Source
  failures are now joined into the returned error while every source still runs.
- **`maxLineBytes` capped nothing.** The limit was checked after the reader had
  already grown its buffer to hold the whole line, so a corrupt log with no
  newline was read into memory in full before being rejected — and rejected
  permanently, since the cursor never advanced. The reader now enforces the cap
  as it accumulates. The split function is a deliberate near-copy of
  `bufio.ScanLines`: the default emits a trailing line that has no terminating
  newline, which is the half-written final record an agent writes live, and
  consuming it would treat a truncated fragment as complete.
- **Accessibility.** `role="button"` on a sortable `<th>` overwrote its implicit
  `columnheader` role, which cost every table on the page its column headers to
  a screen reader and made the `aria-sort` on the same element invalid; the sort
  control is a real button inside the heading now. The project drill-down row
  was click-only, so its entire model and tool breakdown was unreachable by
  keyboard; it is focusable, `Enter`/`Space` operable, and announces its state
  through `aria-expanded`. Numeric columns were right-aligned only in the
  activity table, leaving roughly forty columns of figures left-aligned; the
  copy button's hover state measured 2.14:1 contrast. The session page's stat
  cards used class names the stylesheet defines nowhere, and three colour
  classes the template emits were undefined, so those cards rendered unstyled.
- **The first release would not have published its container image.** The release
  job granted `contents: write` and documented a GHCR push that needs `packages:
  write`, which was never granted: binaries and the GitHub Release would have
  been published, then the image push would have failed with a 403, leaving a
  half-published tag that the workflow is configured not to retry. The release
  footer also advertised a `docker run` line that reached nothing and mounted
  the wrong volume path — undoing the container security default the changelog
  records as fixed.
- The coverage floor sat at 45% against a measured 82.8%, with a comment block
  stale by 35–45 points and its own instruction to raise it unheeded. It is 80%
  now, and the numbers in the comment match reality.
- **Windows was never tested successfully, and the failures hid each other.**
  The `windows-latest` job had been red for the whole life of CI, and each
  defect stopped the job before the next could be seen. Four, in order:
  - Without a `.gitattributes` the Windows runner checked the tree out as CRLF
    and `gofmt` reported all 66 files as unformatted — a repository-wide
    formatting failure that was never one. The working tree is now pinned to LF;
    no stored file changed.
  - PowerShell's native argument passing split
    `-coverprofile=coverage.out` into a bare `.out`, which `go test` tried to
    import as a package. The step now runs under bash, as the formatting step
    already did.
  - `TestDefaultSourcesHonoursTheHomeArgument` compared a `/home/tester` prefix
    against roots `filepath.Join` spells with backslashes. It is about roots
    sitting under the given home, not about which separator spells it.
  - Two Gemini tests faked a home directory by setting `HOME` alone, which
    `os.UserHomeDir` does not read on Windows. **The product code was already
    correct**; the harness was resolving the real profile, finding no
    `.project_root`, and falling back to the project directory name. A shared
    `setFakeHome` now sets both variables.
- The Windows-only permissions test now skips there rather than failing.
  Windows has no Unix permission bits — every file reports `0666` whatever was
  requested, because access is governed by ACLs — so `os.Chmod` succeeds and
  changes nothing. Protecting the database on Windows rests on the profile
  directory's ACL, which is outside this code.

### Added

- A test that serves page loads from a real HTTP server while the scanner commits,
  which is the only one that can check the claim justifying the process split.
- An auth-wiring test that puts the middleware around the actual server mux rather
  than a stub, so a route registered outside the wrapper is caught — `run()`, the
  one place auth is applied, had no coverage at all.
- CI now runs each fuzz target on a budget, so the parsers are exercised by inputs
  the committed corpus does not contain; `govulncheck` and dependency updates; a
  container build on every PR, which catches a `.dockerignore` regression that
  would otherwise drop the price dump and silently report `$0.00` at release time.
- A regression test for each defect above that was previously invisible: the
  `/session` escaping, the model-filtered tool seconds, the retained project
  across an append, the undated Antigravity step, and the DST date bound.

### Changed

- Ten data and money defects, each with a reproduction: `Reprice` dropped
  reasoning tokens (a 12% under-report); a resumed Codex scan dropped its first
  increment; a reported-zero Codex delta fell through to differencing and
  charged 38k cache tokens to a single call; pi and Gemini never billed
  reasoning; zero-token calls were stored with `priced=true`; `TotalTokens`
  excluded reasoning, understating throughput by about 3.8%; `?model=` returned
  HTTP 500; a protobuf field number was truncated; an OpenCode tool part was
  lost; and the database was created world-readable.
- **XSS.** `escapeHtml` in `dashboard.js` round-tripped its argument through
  `textContent`→`innerHTML` and escaped neither quote, so a crafted agent log
  closed a `title=` attribute and injected a live `onmouseover=` handler —
  reproduced in headless Chromium. The escaper now escapes `& < > " '` and the
  backtick by table lookup with no DOM involved, and the inline handlers it
  depended on are gone: every clickable cell is a `data-` attribute read by one
  delegated listener.
- No security headers were sent. Responses now carry `X-Content-Type-Options`,
  `X-Frame-Options`, `Referrer-Policy` and a Content-Security-Policy whose inline
  scripts carry a per-response nonce. The one relaxation is `style-src`, which
  keeps `'unsafe-inline'` because the page writes style attributes.
- The resume command was assembled with double quotes, so a working directory
  containing `"` closed the argument and made the rest a second command. Each
  value is now single-quoted for `sh`.
- The container defaulted to `-addr 0.0.0.0:8753`, so `docker run -p 8753:8753`
  published every project path, session title and cost figure on the network
  with no authentication. The default is now loopback, as the binary's already
  was; publishing takes an explicit bind and a token, together.
- An incremental OpenCode read was committed with `ReplaceSession`, so a
  resumed pass deleted the rows it did not re-read. It appends once a session
  has a non-zero stored cursor, which is the rule the file sources already
  followed.
- `parseInt64` had no overflow guard, so `"9223372036854775808"` wrapped to
  `MinInt64` with no error. A wrapped epoch is still a plausible date, so the
  corruption survived into the report.
- `geminiProjectForPath` read `os.Getenv("HOME")`, which is empty on Windows: the
  lookup produced a CWD-relative path that no installation has, and every Gemini
  session filed itself under its directory name rather than its working
  directory. It now uses `os.UserHomeDir()`.
- `store.Warn`, the hook that reports a database it could not chmod `0600`, was
  never wired up, so the CLI's own logger was bypassed.
- The web source panel duplicated the scanner's six log paths. Both now come
  from `scan.HomeRelativeRoots`, so a hint cannot point somewhere the scanner
  never looked.
- `filterState.Active` was computed and read by nothing. It is now rendered as a
  "Filtered" note beside the Clear link.
- The `go.mod` module path said `tonym128/agent-cost-dashboard`, which did not
  match the git remote, so `go install` and any `go get` of this module failed.
  Corrected to `github.com/tonym128/agent-cost-dashboard`.
- The `LICENSE` named Duncan Ogilvie, the author of the `modernc.org/sqlite`
  dependency, rather than the author of this project.
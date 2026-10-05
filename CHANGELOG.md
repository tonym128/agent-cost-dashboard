# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

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
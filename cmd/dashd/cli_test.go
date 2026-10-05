package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

// TestParseArgs is the table that pins the reported bug: a subcommand after the
// flags used to be swallowed, so `dashd -models x -db y stats` ran a server and
// ignored both flags.
func TestParseArgs(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		wantCmd  string
		wantAddr string
		wantHome string
		wantDB   string
		wantVerb bool
		wantDur  time.Duration
		wantAuth string
		wantErr  string
	}{
		{name: "no arguments at all", argv: nil, wantCmd: defaultCommand},

		{name: "bare command", argv: []string{"scan"}, wantCmd: "scan"},
		{name: "default command spelled out", argv: []string{"serve-and-scan"}, wantCmd: "serve-and-scan"},
		{name: "serve", argv: []string{"serve"}, wantCmd: "serve"},
		{name: "reprice", argv: []string{"reprice"}, wantCmd: "reprice"},
		{name: "stats", argv: []string{"stats"}, wantCmd: "stats"},

		{name: "flag before command", argv: []string{"-verbose", "scan"}, wantCmd: "scan", wantVerb: true},
		{name: "command before flag", argv: []string{"scan", "-verbose"}, wantCmd: "scan", wantVerb: true},

		// The bug, verbatim in shape.
		{
			name:    "flags then command: every flag honoured",
			argv:    []string{"-models", "/nonexistent.json", "-db", "foo.db", "stats"},
			wantCmd: "stats", wantDB: "foo.db", wantHome: defaultHome(t),
		},
		{name: "command then flags", argv: []string{"stats", "-db", "foo.db"}, wantCmd: "stats", wantDB: "foo.db"},
		{
			name:    "command wedged between flags",
			argv:    []string{"-db", "a.db", "serve", "-addr", "127.0.0.1:9999", "-verbose"},
			wantCmd: "serve", wantDB: "a.db", wantAddr: "127.0.0.1:9999", wantVerb: true,
		},
		{
			name:     "equals form is fine too",
			argv:     []string{"-db=a.db", "stats"},
			wantCmd:  "stats",
			wantDB:   "a.db",
			wantHome: defaultHome(t),
		},
		{name: "duration", argv: []string{"-interval", "90s", "serve"}, wantCmd: "serve", wantDur: 90 * time.Second},
		{name: "duration after the command", argv: []string{"serve", "-interval", "90s"}, wantCmd: "serve", wantDur: 90 * time.Second},
		{
			name:    "auth-token before the command",
			argv:    []string{"-interval", "90s", "-auth-token", "s3cret", "serve"},
			wantCmd: "serve", wantDur: 90 * time.Second, wantAuth: "s3cret",
		},
		{name: "auth-token after the command", argv: []string{"serve", "-auth-token", "s3cret"}, wantCmd: "serve", wantAuth: "s3cret"},
		{name: "empty auth-token means off", argv: []string{"-auth-token", "", "serve"}, wantCmd: "serve", wantAuth: ""},

		{name: "version flag", argv: []string{"-version"}, wantCmd: defaultCommand},
		{name: "version flag after a command", argv: []string{"serve", "-version"}, wantCmd: "serve"},

		{name: "typo is an error, not a silent server", argv: []string{"stat"}, wantErr: `unknown command "stat"`},
		{name: "typo after flags is still an error", argv: []string{"-verbose", "scean"}, wantErr: `unknown command "scean"`},
		{name: "a path is not a command", argv: []string{"/usr/local/bin/dashd"}, wantErr: "unknown command"},
		{name: "two commands", argv: []string{"scan", "stats"}, wantErr: `unexpected argument "stats"`},
		{name: "unknown flag", argv: []string{"-nope", "stats"}, wantErr: "flag provided but not defined"},
		{name: "trailing flag with no value", argv: []string{"stats", "-db"}, wantErr: "flag needs an argument"},
	}

	// Note what `-db stats` does, because it is the one genuinely ambiguous
	// input: it sets the database path to "stats" and runs the default command,
	// because a value-taking flag consumes the next argument whatever it looks
	// like. That is how every Unix program behaves, and inventing a rule to
	// guess otherwise would be worse than the ambiguity.

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, cmd, err := parseArgs(tc.argv)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("parseArgs(%q) = error %v, want nil", tc.argv, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("parseArgs(%q) = nil error, want one containing %q", tc.argv, tc.wantErr)
			case tc.wantErr != "":
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseArgs(%q) = error %q, want it to contain %q", tc.argv, err, tc.wantErr)
				}
				// An error must not leave a command behind, or the caller could
				// start a server on the way to reporting the mistake.
				if cmd != "" {
					t.Errorf("parseArgs(%q) returned command %q alongside an error", tc.argv, cmd)
				}
				return
			}

			if cmd != tc.wantCmd {
				t.Errorf("command = %q, want %q", cmd, tc.wantCmd)
			}
			// Only compare fields the case actually pins down; the defaults for
			// the rest depend on the machine's home directory.
			if tc.wantAddr != "" && o.addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", o.addr, tc.wantAddr)
			}
			if tc.wantHome != "" && o.home != tc.wantHome {
				t.Errorf("home = %q, want %q", o.home, tc.wantHome)
			}
			if tc.wantDB != "" && o.db != tc.wantDB {
				t.Errorf("db = %q, want %q", o.db, tc.wantDB)
			}
			if tc.wantAuth != "" && o.authToken != tc.wantAuth {
				t.Errorf("authToken = %q, want %q", o.authToken, tc.wantAuth)
			}
			if tc.wantDur != 0 && o.interval != tc.wantDur {
				t.Errorf("interval = %v, want %v", o.interval, tc.wantDur)
			}
			if o.verbose != tc.wantVerb {
				t.Errorf("verbose = %v, want %v", o.verbose, tc.wantVerb)
			}
		})
	}
}

// TestParseArgsDefaultAddr pins the address that only shows up when a case does
// not override it.
func TestParseArgsDefaultAddr(t *testing.T) {
	o, cmd, err := parseArgs([]string{"stats"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if o.addr != "127.0.0.1:8753" {
		t.Errorf("addr = %q, want 127.0.0.1:8753", o.addr)
	}
	if o.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", o.interval)
	}
	if cmd != "stats" {
		t.Errorf("command = %q, want stats", cmd)
	}
}

// TestParseArgsHelp checks that -h surfaces as flag.ErrHelp, which runCLI turns
// into usage and exit 0 rather than an error.
func TestParseArgsHelp(t *testing.T) {
	_, _, err := parseArgs([]string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h gave %v, want flag.ErrHelp", err)
	}
}

func TestParseArgsErrorsAreSelfDescribing(t *testing.T) {
	_, _, err := parseArgs([]string{"stat"})
	if err == nil {
		t.Fatal("want an error")
	}
	// The message has to name what was available, or the user is left guessing
	// which of five commands they meant.
	for _, want := range commands {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPrintUsageMentionsEverything guards the two ways usage text rots: a
// command that was added without being listed, and a flag that was added
// without being documented.
func TestPrintUsageMentionsEverything(t *testing.T) {
	var sb strings.Builder
	printUsage(&sb)
	got := sb.String()
	for _, c := range commands {
		if !strings.Contains(got, c) {
			t.Errorf("usage does not document command %q", c)
		}
	}
	for _, f := range []string{"-db", "-addr", "-interval", "-home", "-models", "-opencode", "-verbose", "-auth-token", "-version"} {
		if !strings.Contains(got, f) {
			t.Errorf("usage does not document flag %q", f)
		}
	}
	if strings.Contains(got, "server.URL") {
		t.Error("usage leaks a placeholder")
	}
}

func defaultHome(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	return h
}

// TestRunCLIUnknownCommand is the end-to-end half of the parsing tests: a typo
// must exit non-zero and say so. Before this change it fell through to
// serve-and-scan, so a mistyped cron entry started an unattended server that
// scanned the user's home directory.
func TestRunCLIUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{"bogus-subcommand"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("exit code 0 for an unknown command; want non-zero")
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus-subcommand"`) {
		t.Errorf("stderr = %q, want it to name the unknown command", &stderr)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", &stdout)
	}
}

func TestRunCLIBadFlagPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"-nope"}, &stdout, &stderr); code == 0 {
		t.Fatal("exit code 0 for an unknown flag, want non-zero")
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr = %q, want usage alongside the error", &stderr)
	}
}

func TestRunCLIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d, want 0 for -h", code)
	}
	if !strings.Contains(stderr.String(), "Commands:") {
		t.Errorf("stderr = %q, want usage", &stderr)
	}
}

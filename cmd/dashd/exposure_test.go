package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestExposedToNetwork(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8753", false},
		{"127.0.0.1:0", false},
		{"localhost:8753", false},
		{"LocalHost:8753", false},
		{"[::1]:8753", false},
		{"0.0.0.0:8753", true},
		{":8753", true},
		{"[::]:8753", true},
		{"192.168.1.10:8753", true},
		{"10.0.0.5:8753", true},
		{"[2001:db8::1]:8753", true},
		// A hostname is not an IP, so it is not provably loopback; assume the
		// worst rather than quietly reassuring.
		{"dashd.example.com:8753", true},
		{"not-an-address", true},
	}
	for _, tc := range tests {
		if got := exposedToNetwork(tc.addr); got != tc.want {
			t.Errorf("exposedToNetwork(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestLogExposure checks the operator-facing half of the auth decision: the one
// case that must be loud is a public bind with no token.
func TestLogExposure(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		token      string
		wantLevel  slog.Level
		wantSubstr string
		wantSilent bool
	}{
		{
			// The normal case, and the reason a warning here would be a
			// liability: it would teach the operator to skip warnings.
			name: "loopback and no token says nothing", addr: "127.0.0.1:8753", wantSilent: true,
		},
		{
			name: "public bind with no token warns loudly",
			addr: "0.0.0.0:8753", wantLevel: slog.LevelWarn,
			wantSubstr: "NO authentication",
		},
		{
			name: "public bind with a token logs protection",
			addr: "0.0.0.0:8753", token: "s3cret", wantLevel: slog.LevelInfo,
			wantSubstr: "protected by -auth-token",
		},
		{
			name: "loopback with a token notes it is on",
			addr: "127.0.0.1:8753", token: "s3cret", wantLevel: slog.LevelInfo,
			wantSubstr: "authentication enabled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			logExposure(options{addr: tc.addr, authToken: tc.token}, log)
			got := buf.String()

			if tc.wantSilent {
				if got != "" {
					t.Errorf("log = %q, want nothing for a loopback bind with no token", got)
				}
				return
			}
			if tc.wantSubstr != "" && !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("log = %q, want it to contain %q", got, tc.wantSubstr)
			}
			// The token value must never reach the log; that is why the message
			// says "protected" instead of quoting by what.
			if tc.token != "" && strings.Contains(got, tc.token) {
				t.Errorf("log leaked the token: %q", got)
			}
			if strings.Contains(got, "level=WARN") != (tc.wantLevel == slog.LevelWarn) {
				t.Errorf("log = %q, want warn=%v", got, tc.wantLevel == slog.LevelWarn)
			}
		})
	}
}

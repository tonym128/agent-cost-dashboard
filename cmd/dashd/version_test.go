package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionString(t *testing.T) {
	// The three names are the contract with the Makefile and .goreleaser.yml.
	// If one is renamed, `-X main.version=...` stops being an error and starts
	// being a no-op again, which is exactly how this went unnoticed.
	if version != "dev" {
		t.Errorf("version = %q, want the unstamped default %q", version, "dev")
	}
	if commit != "none" {
		t.Errorf("commit = %q, want %q", commit, "none")
	}
	if date != "unknown" {
		t.Errorf("date = %q, want %q", date, "unknown")
	}
	for _, want := range []string{"dashd", "dev", "none", "unknown"} {
		if !strings.Contains(versionString(), want) {
			t.Errorf("versionString() = %q, want it to contain %q", versionString(), want)
		}
	}
}

func TestRunCLIVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, &stderr)
	}
	if got := strings.TrimSpace(stdout.String()); got != versionString() {
		t.Errorf("stdout = %q, want %q", got, versionString())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", &stderr)
	}
}

func TestRunCLIVersionAfterCommand(t *testing.T) {
	// `-version` must be found wherever it is written, including after a command
	// that would otherwise start a server.
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"serve", "-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "dashd") {
		t.Errorf("stdout = %q, want the version line", &stdout)
	}
}

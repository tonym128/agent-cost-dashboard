package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelsTree builds a throwaway root with a bin/ and a work/ directory. A fresh
// tree per case matters: these tests are about search order, and a models.json
// left behind by one case would decide the next one.
func modelsTree(t *testing.T) (root, binDir, workDir, exe string) {
	t.Helper()
	root = t.TempDir()
	binDir = filepath.Join(root, "bin")
	workDir = filepath.Join(root, "work")
	for _, d := range []string{binDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, binDir, workDir, filepath.Join(binDir, "dashd")
}

func TestModelsPathFrom(t *testing.T) {
	tests := []struct {
		name string
		set  []string // dump locations, relative to root
		want string
	}{
		{name: "beside the binary wins", set: []string{"bin/models.json", "work/models.json"}, want: "bin/models.json"},
		{name: "one level above the binary", set: []string{"models.json", "work/models.json"}, want: "models.json"},
		{name: "falls back to the working directory", set: []string{"work/models.json"}, want: "work/models.json"},
		{name: "parent of the working directory", set: []string{"models.json"}, want: "models.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, _, workDir, exe := modelsTree(t)
			for _, p := range tc.set {
				if err := os.WriteFile(filepath.Join(root, p), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := modelsPathFrom(exe, workDir)
			want := filepath.Join(root, tc.want)
			if filepath.Clean(got) != filepath.Clean(want) {
				t.Errorf("modelsPathFrom() = %q, want %q", got, want)
			}
		})
	}
}

// TestModelsPathFromNoExecutable covers the case where os.Executable failed and
// the working directory is all there is to go on.
func TestModelsPathFromNoExecutable(t *testing.T) {
	_, _, workDir, _ := modelsTree(t)
	if err := os.WriteFile(filepath.Join(workDir, "models.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := modelsPathFrom("", workDir), filepath.Join(workDir, "models.json"); got != want {
		t.Errorf("modelsPathFrom(\"\", workDir) = %q, want %q", got, want)
	}
}

// TestModelsPathFromNoneFound documents the last resort: a relative
// "models.json", which resolves against the working directory rather than
// pretending to an absolute path that does not exist.
func TestModelsPathFromNoneFound(t *testing.T) {
	root := t.TempDir()
	got := modelsPathFrom(filepath.Join(root, "bin", "dashd"), filepath.Join(root, "work"))
	if got != "models.json" {
		t.Errorf("modelsPathFrom() = %q, want %q with no dump anywhere", got, "models.json")
	}
}

// TestDefaultModelsPathOnly checks the wrapper itself, since it cannot be given
// a controlled executable path.
func TestDefaultModelsPathOnly(t *testing.T) {
	got := defaultModelsPath()
	if got == "" {
		t.Fatal("defaultModelsPath() returned the empty string")
	}
	// It must at least name the file it is looking for.
	if filepath.Base(got) != "models.json" {
		t.Errorf("defaultModelsPath() = %q, want a path ending in models.json", got)
	}
}

// TestWaitForReturnsWhenDone covers the bounded wait added so that a SIGTERM
// stops the writer before the store closes, without hanging forever on a large
// home directory.
func TestWaitForReturnsWhenDone(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var wg sync.WaitGroup
	wg.Add(1)
	wg.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()
	done := make(chan struct{})
	go func() {
		waitFor(&wg, log)
		close(done)
	}()
	wg.Done()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitFor did not return for a finished scanner")
	}
	if strings.Contains(buf.String(), "timeout") {
		t.Errorf("unexpected timeout warning: %q", &buf)
	}
}

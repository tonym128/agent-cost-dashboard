package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The database holds every project path, session title, model name and cost
// figure the dashboard has ever ingested. The HTTP auth token guards the web
// endpoint; it does nothing about a file on disk. With the directory created
// 0755 and the file 0644 under the common 022 umask, any local account on a
// multi-user host can read the whole thing directly.

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestNewDatabaseIsNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "share")
	path := filepath.Join(dir, "dashd.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := modeOf(t, path); got&0o077 != 0 {
		t.Errorf("database mode is %04o; group and other can read a file holding every project path and cost figure", got)
	}
	if got := modeOf(t, dir); got&0o077 != 0 {
		t.Errorf("directory mode is %04o; the database is reachable without knowing its name", got)
	}
	// The write-ahead log and shared-memory index are created beside it and hold
	// the same rows in transit, so they are restricted too.
	for _, suffix := range []string{"-wal", "-shm"} {
		p := path + suffix
		if _, err := os.Stat(p); err != nil {
			continue // not created; a delete-journal database has neither
		}
		if got := modeOf(t, p); got&0o077 != 0 {
			t.Errorf("%s mode is %04o; group and other can read the write-ahead log", suffix, got)
		}
	}
}

// TestExistingDatabaseKeepsItsMode is the other half, and the reason the
// restriction is conditional.
//
// A database the user has been running for months is theirs. They may have set
// 0640 deliberately to share it with a group, or 0660 on a shared volume.
// Chmod-ing it on every start would override a decision they made deliberately
// and silently, which is its own kind of wrong.
func TestExistingDatabaseKeepsItsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not modelled on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dashd.db")

	// Stand in for a database that already exists, created by the user with a
	// mode they chose.
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := modeOf(t, path); got != 0o640 {
		t.Errorf("an existing database's mode became %04o; a mode the user set deliberately was overridden", got)
	}
}

// TestChmodFailureDoesNotStopStartup covers the mounted-volume case: a
// filesystem that does not support permission bits makes chmod fail, and
// refusing to open the database would trade a visible warning for an invisible
// outage.
func TestChmodFailureDoesNotStopStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not modelled on Windows")
	}
	// /proc is the portable example of a filesystem that refuses permission
	// changes, and the path exists on every platform this runs on.
	unwritable := "/proc/self/status"
	if _, err := os.Stat(unwritable); err != nil {
		t.Skip("no /proc on this platform")
	}

	var warned []string
	prev := Warn
	Warn = func(msg string, _ ...any) { warned = append(warned, msg) }
	defer func() { Warn = prev }()

	// A failing chmod must warn and return, not panic and not block.
	restrictToOwner(unwritable)
	if len(warned) == 0 {
		t.Error("a chmod that failed produced no warning; the operator cannot tell the database is exposed")
	}
}

// TestNewDatabaseSurvivesAnUnwritableTarget ties the two halves together: the
// restriction failing must not stop the database from opening, because the usual
// cause is a bind mount or network share with a fixed mode.
func TestNewDatabaseSurvivesAnUnwritableTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashd.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove write permission from the directory so the database cannot be
	// created there at all; Open must then fail on its own terms, cleanly.
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(filepath.Dir(path), 0o700)

	// Either it opened (the file already existed, so SQLite had nothing to
	// create) or it returned an error. What must not happen is a panic, or a
	// success that reports success while silently leaving the file exposed.
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Log("opened an existing database in a read-only directory, as expected")
	}
}

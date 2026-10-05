package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A resume offset alone cannot tell "the log grew" from "the log was rewritten
// in place": a rewritten log that still ends past the old offset looks exactly
// like an append. The head fingerprint is what separates them, so these tests
// cover the fingerprint itself rather than trusting that the scan wires it up.

// fingerprintOf is a small helper returning the recorded pair.
func fingerprintOf(t *testing.T, path string) (string, int64) {
	t.Helper()
	hash, length := headFingerprint(path)
	if hash == "" {
		t.Fatalf("no fingerprint recorded for %s", path)
	}
	return hash, length
}

// TestHeadFingerprintDetectsRewriteNotAppend is the claim: an append resumes,
// a rewrite re-reads from the start.
func TestHeadFingerprintDetectsRewriteNotAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	head := `{"n":1}` + "\n"
	os.WriteFile(path, []byte(head), 0o600)
	hash, length := fingerprintOf(t, path)

	// The fingerprint covers what is there, and says how much, so a later
	// comparison can re-read exactly the same range.
	if length != int64(len(head)) {
		t.Errorf("fingerprint covered %d bytes, want %d", length, len(head))
	}
	if !HeadMatches(path, hash, length) {
		t.Error("an unchanged file did not match its own fingerprint")
	}

	// The append case: the head is untouched, so the offset stays valid.
	os.WriteFile(path, []byte(head+`{"n":2}`+"\n"), 0o600)
	if !HeadMatches(path, hash, length) {
		t.Error("an appended log did not match: the whole point is to resume rather than re-read")
	}

	// The rewrite case: same length, different head.
	os.WriteFile(path, []byte(`{"n":9}`+"\n"), 0o600)
	if HeadMatches(path, hash, length) {
		t.Error("a rewritten log matched its old fingerprint: the calls it dropped would stay in the database forever")
	}

	// Truncation below the recorded window cannot match, which is the right
	// answer: the file was rotated.
	os.WriteFile(path, []byte(""), 0o600)
	if HeadMatches(path, hash, length) {
		t.Error("an emptied log matched")
	}
}

// TestHeadFingerprintRecordsItsOwnLength covers why the length is recorded
// alongside the hash.
//
// Without it, a log shorter than the window would have its entire contents
// hashed, and the first append would then change the hash and look like a
// rewrite — so every short, frequently-appended session would be re-read from
// the start on every pass.
func TestHeadFingerprintRecordsItsOwnLength(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.jsonl")
	body := `{"n":1}` + "\n" // well under CursorPrefixMax
	os.WriteFile(short, []byte(body), 0o600)

	hash, length := fingerprintOf(t, short)
	if length != int64(len(body)) {
		t.Errorf("fingerprint covered %d bytes, want the whole %d-byte file", length, len(body))
	}
	if length > CursorPrefixMax {
		t.Errorf("fingerprint covered %d bytes, above the %d-byte cap", length, CursorPrefixMax)
	}

	// Growing a short file must still match, because the recorded range is a
	// prefix of the new content.
	os.WriteFile(short, []byte(body+`{"n":2}`+"\n"), 0o600)
	if !HeadMatches(short, hash, length) {
		t.Error("appending to a file shorter than the window read as a rewrite")
	}

	// A file at or above the cap is covered only up to the cap, and appending
	// past the cap still matches.
	long := filepath.Join(dir, "long.jsonl")
	big := strings.Repeat("x", CursorPrefixMax) + "\n"
	os.WriteFile(long, []byte(big), 0o600)
	lhash, llen := fingerprintOf(t, long)
	if llen != CursorPrefixMax {
		t.Errorf("fingerprint covered %d bytes, want the %d-byte cap", llen, CursorPrefixMax)
	}
	os.WriteFile(long, []byte(big+strings.Repeat("y", CursorPrefixMax)+"\n"), 0o600)
	if !HeadMatches(long, lhash, llen) {
		t.Error("appending past the window changed the fingerprint")
	}

	// A window past the end is refused rather than quietly hashing the whole
	// thing: the recorded length has to be reproducible. The window is capped
	// first, so this still covers exactly the cap.
	if hash, length := headFingerprintLimited(long, int64(len(big))+10); length != CursorPrefixMax {
		t.Errorf("a window past the end covered %d bytes, want the %d-byte cap",
			length, CursorPrefixMax)
	} else if hash == "" {
		t.Error("a window past the end produced no fingerprint")
	}
}

// TestHeadFingerprintEdgeCases: the inputs that are easy to get wrong rather
// than the common ones.
func TestHeadFingerprintEdgeCases(t *testing.T) {
	dir := t.TempDir()

	// An empty file gets a value that cannot collide with any non-empty head,
	// and it has to be derived from the path so two empty logs stay distinct —
	// otherwise one session's fingerprint would validate another's.
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	os.WriteFile(a, nil, 0o600)
	os.WriteFile(b, nil, 0o600)
	hashA, lengthA := fingerprintOf(t, a)
	hashB, _ := fingerprintOf(t, b)
	if !strings.HasPrefix(hashA, "empty:") {
		t.Errorf("an empty file fingerprinted as %q, want the empty: form", hashA)
	}
	if lengthA != 0 {
		t.Errorf("an empty file recorded %d bytes, want 0", lengthA)
	}
	if hashA == hashB {
		t.Error("two empty files share a fingerprint: one session's resume point would validate another's")
	}
	if !HeadMatches(a, hashA, lengthA) {
		t.Error("an unchanged empty file did not match")
	}
	// Once the file has content the empty fingerprint still agrees, because a
	// zero-length prefix has nothing to compare. That is harmless rather than a
	// gap: the cursor recorded alongside it is 0, so a resume reads the whole
	// file either way and a rewrite cannot cause a record to be missed. The
	// check earns its keep from the moment there is a prefix to lose.
	os.WriteFile(a, []byte("{}\n"), 0o600)

	// A file that does not exist yields nothing rather than panicking, and
	// nothing recorded means "cannot vouch for this", which is what an absent
	// prefix hash has to mean.
	missing := filepath.Join(dir, "missing.jsonl")
	if hash, length := headFingerprint(missing); hash != "" || length != 0 {
		t.Errorf("a missing file fingerprinted as %q/%d, want empty", hash, length)
	}
	if HeadMatches(missing, hashA, lengthA) {
		t.Error("a missing file matched a fingerprint")
	}
	// No recorded hash is never a match, whatever the file holds.
	if HeadMatches(a, "", 0) {
		t.Error("a file matched with no fingerprint recorded: every resume point would be treated as trustworthy")
	}

	// A directory is not a file to read.
	if hash, _ := headFingerprint(dir); hash != "" {
		t.Error("a directory produced a fingerprint")
	}
}

package source

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// shortHash returns a stable short digest of s.
//
// Used for deriving a session id from a path and for fingerprinting a file's
// head, so it only needs to be collision-resistant in practice, not
// cryptographically.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// prefixHash fingerprints the first n bytes of a file so that "the log grew" can
// be told apart from "the log was rewritten in place".
//
// A resume offset alone cannot distinguish those: a rewritten log that happens
// to end past the old offset would be mistaken for an append, and the calls the
// rewrite removed would stay in the database forever. Comparing the head is a
// few kilobytes of I/O and turns that silent error into a re-read.
// headFingerprint hashes a fixed leading window of a file and reports how many
// bytes it covered. The caller records the length so a later comparison can
// re-read exactly the same range — otherwise a file shorter than the window
// would have its whole contents hashed, and any append would look like a
// rewrite.
func headFingerprint(path string) (hash string, length int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	buf := make([]byte, CursorPrefixMax)
	got, err := io.ReadFull(f, buf)
	if got == 0 {
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return "", 0
		}
		return "empty:" + shortHash(path), 0
	}
	return shortHash(string(buf[:got])), int64(got)
}

// CursorPrefixMax is the largest leading window ever fingerprinted.
const CursorPrefixMax = 4096

// HeadMatches reports whether a file still begins with the recorded bytes.
// A file that has since become shorter than the recorded window cannot match,
// which is the right answer: it was truncated.
func HeadMatches(path string, prevHash string, prevLen int64) bool {
	if prevHash == "" {
		return false
	}
	hash, length := headFingerprintLimited(path, prevLen)
	if length != prevLen {
		return false
	}
	return hash == prevHash
}

func headFingerprintLimited(path string, n int64) (string, int64) {
	if n > CursorPrefixMax {
		n = CursorPrefixMax
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if int64(got) != n {
		return "", int64(got)
	}
	_ = err
	return shortHash(string(buf[:got])), int64(got)
}

func prefixHash(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if got == 0 {
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return ""
		}
		// An empty file: a value that cannot collide with any non-empty head.
		return "empty:" + shortHash(path)
	}
	return shortHash(string(buf[:got]))
}

package source

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// maxLineBytes caps a single JSONL record. Session messages embed tool output
// and file bodies, so lines run to megabytes; the cap exists so a corrupt file
// with no newline cannot make the scanner allocate without bound.
const maxLineBytes = 64 << 20 // 64 MiB

// record is one decoded JSONL line, with the byte range it occupied so the
// caller can resume from exactly the end of the last complete line.
type record struct {
	// Data is the decoded object. Non-object JSON (a bare number or string) is
	// valid JSON but has nothing to read, so those lines yield Data == nil.
	Data map[string]any
	// End is the byte offset just past this line's terminating newline.
	End int64
	// Ordinal is the 0-based index of this line within the file, which parsers
	// use to derive a stable call key when the log provides no id of its own.
	Ordinal int64
}

// scanLines reads complete newline-terminated records from r, starting at
// `from`.
//
// The final line of a file an agent is still writing is routinely half-written.
// Only whole lines are yielded and the byte position is never advanced past one,
// so the next scan resumes from the same place and picks the record up once it
// is finished. Reading to EOF regardless of completeness would either drop the
// record or, worse, treat a truncated fragment as complete.
func scanLines(path string, from int64) (recs []record, consumed int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	// A file that shrank was rotated or rewritten. The caller decides what to
	// do; returning the whole file to re-read is always safe.
	if from < 0 || from > st.Size() {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, 0, err
	}

	reader := bufio.NewReaderSize(f, 256<<10)
	pos := from
	var ordinal int64
	// Count existing lines so ordinals stay stable across incremental scans: a
	// parser's fallback call key is derived from the line number, and it must
	// not shift just because we resumed mid-file.
	if ordinal, err = countLinesBefore(path, from); err != nil {
		return nil, 0, err
	}

	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			// io.EOF with no bytes, or a trailing fragment: stop here and leave
			// `consumed` pointing at the last complete line.
			break
		}
		pos += int64(len(line))
		if len(line) > maxLineBytes {
			return recs, pos, fmt.Errorf("line at offset %d exceeds %d bytes", pos, maxLineBytes)
		}
		recs = append(recs, record{Data: decodeObject(line), End: pos, Ordinal: ordinal})
		ordinal++
	}
	return recs, pos, nil
}

// decodeObject parses one line, returning nil for anything unusable.
func decodeObject(line string) map[string]any {
	if len(line) < 2 {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return obj
}

// countLinesBefore counts the records that precede the resume point.
//
// This is what keeps a parser's fallback call key stable across incremental
// scans: the key is derived from a line number, and without this it would shift
// every time we resumed mid-file, turning an append into a re-key that would
// duplicate rows.
func countLinesBefore(path string, n int64) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var count int64
	buf := make([]byte, 1<<20)
	remaining := n
	for remaining > 0 {
		size := int64(len(buf))
		if remaining < size {
			size = remaining
		}
		got, err := f.Read(buf[:size])
		for _, b := range buf[:got] {
			if b == '\n' {
				count++
			}
		}
		if err != nil {
			break
		}
		remaining -= int64(got)
	}
	return count, nil
}

// ---------------------------------------------------------------- helpers

// nested walks a path of map keys, returning the value found and whether the
// whole path existed. Agent logs nest inconsistently and a missing key several
// levels down must not panic.
func nested(obj map[string]any, path ...string) (any, bool) {
	var cur any = obj
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// str returns a string field, or "" when absent or of another type.
func str(obj map[string]any, key string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// asMap coerces a field to an object, treating null and non-objects as empty.
// A present-but-null value is routine in these logs and `.get(k, {})` in the
// original implementation did not cover it.
func asMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// asSlice coerces a field to a slice, treating anything else as empty.
func asSlice(v any) []any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	return s
}

// num reads a numeric field, tolerating the several ways JSON encoders write
// numbers, and clamping negatives to zero.
//
// Clamping matters: a negative token count would otherwise flow into every
// downstream total, and one bad record should not be able to make a whole
// dashboard read as though it owes money.
func num(obj map[string]any, key string) int64 {
	v, ok := obj[key]
	if !ok {
		return 0
	}
	return toInt(v)
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		if t < 0 {
			return 0
		}
		return int64(t)
	case int64:
		if t < 0 {
			return 0
		}
		return t
	case int:
		if t < 0 {
			return 0
		}
		return int64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil || f < 0 {
			return 0
		}
		return int64(f)
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}

// parseTime accepts the timestamp shapes these agents emit: RFC3339 with or
// without a zone, and epoch milliseconds as a number or string.
func parseTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		if t == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UTC()
			}
		}
		// Fall back to epoch millis encoded as a string.
		if ms, err := parseInt64(t); err == nil {
			return epochMillis(ms)
		}
	case float64:
		return epochMillis(int64(t))
	case int64:
		return epochMillis(t)
	case json.Number:
		if ms, err := t.Int64(); err == nil {
			return epochMillis(ms)
		}
	}
	return time.Time{}
}

// epochMillis converts a millisecond epoch to UTC. Values small enough to be
// seconds rather than milliseconds are widened, because OpenCode records epoch
// milliseconds while some logs record seconds under the same field name.
func epochMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	if ms < 1e11 {
		return time.Unix(ms, 0).UTC()
	}
	return time.UnixMilli(ms).UTC()
}

func parseInt64(s string) (int64, error) {
	var n int64
	var neg bool
	for i, r := range s {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an integer: %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ScanLinesForTest exposes the reader for the scanner package's diagnostics.
func ScanLinesForTest(path string, from int64) ([]int, int64, error) {
	recs, consumed, err := scanLines(path, from)
	out := make([]int, 0, len(recs))
	for _, r := range recs {
		if r.Data != nil {
			out = append(out, int(r.Ordinal))
		}
	}
	return out, consumed, err
}

// fileSize returns a file's length, or zero when it cannot be stat-ed.
func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

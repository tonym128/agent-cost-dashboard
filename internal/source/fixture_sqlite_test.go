package source

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures under testdata/ are committed as JSON rather than as binary
// databases or pre-encoded blobs, and this file turns them into the SQLite files
// and protobuf payloads the parsers actually read.
//
// The reason is that a fixture nobody can read is a fixture nobody can update.
// A format change shows up as a failing test, and fixing it means editing a
// field in a diff rather than regenerating a blob nobody can inspect. The schema
// the parser depends on therefore lives here, in the same commit as the tests
// that break when it changes.

// testdataPath resolves a path under testdata.
func testdataPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("testdata", rel)
}

// readFixture decodes a committed fixture.
func readFixture(t *testing.T, rel string, into any) {
	t.Helper()
	raw, err := os.ReadFile(testdataPath(t, rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode fixture %s: %v", rel, err)
	}
}

// copyFixtureTo copies a committed fixture into a temporary directory.
//
// Every test parses from t.TempDir() rather than from testdata directly: a
// parser must never be able to write to the fixtures, and a test that mutated a
// committed file would pass locally and fail in CI.
func copyFixtureTo(t *testing.T, rel string) string {
	t.Helper()
	src := testdataPath(t, rel)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(rel))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// ---------------------------------------------------------------- OpenCode

type openCodeFixture struct {
	Sessions []struct {
		ID          string `json:"id"`
		Directory   string `json:"directory"`
		Title       string `json:"title"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		TimeCreated int64  `json:"time_created"`
		TimeUpdated int64  `json:"time_updated"`
		Messages    []struct {
			ID          string `json:"id"`
			TimeCreated int64  `json:"time_created"`
			TimeUpdated int64  `json:"time_updated"`
			Data        string `json:"-"`
			DataRaw     any    `json:"data"`
			Parts       []any  `json:"parts"`
		} `json:"messages"`
	} `json:"sessions"`
}

// buildOpenCodeDB writes the fixture as the SQLite database the OpenCode parser
// reads, and returns its path.
//
// Only the columns the parser selects are created. The real database has
// several dozen more, and adding them here would make the fixture look like it
// tests schema compatibility when what it tests is that a *change* to the
// columns we do read fails loudly.
func buildOpenCodeDB(t *testing.T) string {
	t.Helper()
	var fx openCodeFixture
	readFixture(t, "opencode/opencode_sessions.json", &fx)

	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY, directory TEXT, title TEXT, agent TEXT, model TEXT,
			time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	for _, s := range fx.Sessions {
		if _, err := db.Exec(
			`INSERT INTO session (id, directory, title, agent, model, time_created, time_updated)
			 VALUES (?,?,?,?,?,?,?)`,
			s.ID, s.Directory, s.Title, s.Agent, s.Model, s.TimeCreated, s.TimeUpdated); err != nil {
			t.Fatalf("insert session: %v", err)
		}
		for _, m := range s.Messages {
			data, err := json.Marshal(m.DataRaw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(
				`INSERT INTO message (id, session_id, time_created, time_updated, data)
				 VALUES (?,?,?,?,?)`,
				m.ID, s.ID, m.TimeCreated, m.TimeUpdated, string(data)); err != nil {
				t.Fatalf("insert message: %v", err)
			}
			for pi, p := range m.Parts {
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
					 VALUES (?,?,?,?,?,?)`,
					partID(m.ID, pi), m.ID, s.ID, m.TimeUpdated, m.TimeUpdated, string(raw)); err != nil {
					t.Fatalf("insert part: %v", err)
				}
			}
		}
	}
	return path
}

func partID(messageID string, i int) string {
	return "prt_" + messageID + "_" + string(rune('a'+i))
}

// ---------------------------------------------------------------- Antigravity

type antigravityFixture struct {
	// Comment records where the fixture came from. The reference test reads it,
	// so the provenance travels with the numbers instead of living in a commit
	// message nobody reading the test will see.
	Comment   string `json:"comment"`
	Workspace string `json:"workspace"`
	Epoch     int64  `json:"epoch"`
	Steps     []struct {
		StepType int    `json:"step_type"`
		Status   int    `json:"status"`
		Start    int64  `json:"start"`
		Duration *int64 `json:"duration"`
		Tool     string `json:"tool"`
		// TruncatedUsage encodes a usage sub-message whose declared length runs
		// past the end of the blob, and VarintOverrun one whose varint never
		// terminates. Both are malformed in the way a half-written or corrupt
		// blob is, and both must contribute no tokens at all.
		TruncatedUsage *struct {
			Model     int64 `json:"model"`
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			CacheRead int64 `json:"cache_read"`
			Reasoning int64 `json:"reasoning"`
		} `json:"truncated_usage"`
		VarintOverrun *struct {
			Model int64 `json:"model"`
		} `json:"varint_overrun"`
		ToolWithoutName bool `json:"tool_without_name"`
		Usage           *struct {
			Model     int64 `json:"model"`
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			CacheRead int64 `json:"cache_read"`
			Reasoning int64 `json:"reasoning"`
		} `json:"usage"`
	} `json:"steps"`
}

// buildAntigravityDB writes the fixture as a conversation database and returns
// its path.
func buildAntigravityDB(t *testing.T, name string) string {
	t.Helper()
	var fx antigravityFixture
	readFixture(t, "antigravity/antigravity_steps.json", &fx)

	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The real schema, minus the tables the parser never reads.
	for _, s := range []string{
		`CREATE TABLE steps (
			idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0,
			status INTEGER NOT NULL DEFAULT 0, has_subtrajectory numeric NOT NULL DEFAULT false,
			metadata blob, error_details blob, permissions blob, task_details blob,
			render_info blob, step_payload blob, step_format INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE trajectory_meta (
			trajectory_id text, cascade_id text, trajectory_type integer, source integer,
			PRIMARY KEY (trajectory_id))`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	payload := agyWorkspacePayload(fx.Workspace)
	for i, s := range fx.Steps {
		var metadata []byte
		start := fx.Epoch + s.Start
		switch {
		case s.Usage != nil:
			metadata = agyStepMetadata(start, s.Duration, s.Usage.Model, s.Usage.Input,
				s.Usage.Output, s.Usage.CacheRead, s.Usage.Reasoning)
		case s.TruncatedUsage != nil:
			usage := protoVarint(1, s.TruncatedUsage.Model)
			usage = append(usage, protoVarint(2, s.TruncatedUsage.Input)...)
			usage = append(usage, protoVarint(3, s.TruncatedUsage.Output)...)
			metadata = append(protoBytes(1, protoVarint(1, start)), protoBytes(9, usage)...)
			// Cut the blob in the middle of the usage sub-message's declared
			// length, which is what a partially flushed blob looks like.
			metadata = metadata[:len(metadata)-1]
		case s.VarintOverrun != nil:
			meta := protoBytes(1, protoVarint(1, start))
			usage := protoKey(1, 0)
			for i := 0; i < maxVarintBytes+2; i++ {
				usage = append(usage, 0xff) // a varint that never terminates
			}
			metadata = append(meta, protoBytes(9, usage)...)
		case s.ToolWithoutName:
			metadata = agyToolMetadata(start, s.Duration, "")
		case s.Tool != "":
			metadata = agyToolMetadata(start, s.Duration, s.Tool)
		}
		if _, err := db.Exec(
			`INSERT INTO steps (idx, step_type, status, metadata, step_payload)
			 VALUES (?,?,?,?,?)`,
			i, s.StepType, s.Status, metadata, payload); err != nil {
			t.Fatalf("insert step %d: %v", i, err)
		}
	}
	return path
}

// agyWorkspacePayload encodes the conversation's working directory the way the
// parser looks for it: step_payload, field 28, whose field 2 is the path.
func agyWorkspacePayload(dir string) []byte {
	return protoBytes(28, protoBytes(2, []byte(dir)))
}

func agyStepMetadata(start int64, duration *int64, model, input, output, cacheRead, reasoning int64) []byte {
	usage := protoVarint(1, model)
	usage = append(usage, protoVarint(2, input)...)
	usage = append(usage, protoVarint(3, output)...)
	usage = append(usage, protoVarint(5, cacheRead)...)
	usage = append(usage, protoVarint(9, reasoning)...)

	meta := protoBytes(1, protoVarint(1, start))
	if duration != nil {
		meta = append(meta, protoBytes(8, protoVarint(1, start+*duration))...)
	}
	return append(meta, protoBytes(9, usage)...)
}

func agyToolMetadata(start int64, duration *int64, name string) []byte {
	// Field 4 holds the tool descriptor; the parser reads the name from its
	// field 2.
	desc := protoBytes(1, []byte("call_test"))
	desc = append(desc, protoBytes(2, []byte(name))...)
	meta := protoBytes(1, protoVarint(1, start))
	if duration != nil {
		meta = append(meta, protoBytes(8, protoVarint(1, start+*duration))...)
	}
	return append(meta, protoBytes(4, desc)...)
}

// protoKey encodes the field key that precedes every value: the field number
// shifted up three bits, with the wire type in the low three.
func protoKey(field int32, wire int) []byte {
	key := uint64(field)<<3 | uint64(wire)
	var out []byte
	for key >= 0x80 {
		out = append(out, byte(key)|0x80)
		key >>= 7
	}
	return append(out, byte(key))
}

// protoVarint encodes one varint field: the key, then the value in base 128.
func protoVarint(field int32, v int64) []byte {
	out := protoKey(field, 0)
	var buf [10]byte
	n := 0
	u := uint64(v)
	for {
		b := byte(u & 0x7f)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		buf[n] = b
		n++
		if u == 0 {
			break
		}
	}
	return append(out, buf[:n]...)
}

// protoBytes encodes one length-delimited field.
func protoBytes(field int32, payload []byte) []byte {
	// The key is a varint too: field 28 is 0xe2 0x01, not a single truncated
	// byte. Encoding it as one byte is exactly the kind of plausible-looking
	// wrong blob the reader is supposed to reject.
	out := protoKey(field, 2)
	var buf [10]byte
	n := 0
	l := uint64(len(payload))
	for {
		b := byte(l & 0x7f)
		l >>= 7
		if l != 0 {
			b |= 0x80
		}
		buf[n] = b
		n++
		if l == 0 {
			break
		}
	}
	out = append(out, buf[:n]...)
	return append(out, payload...)
}

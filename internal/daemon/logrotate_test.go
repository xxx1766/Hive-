package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rotating log contract: (a) small writes just accumulate; (b) once
// a would-be write crosses maxBytes, the current file is renamed to
// <path>.1 and a fresh file starts; (c) only one backup level is kept
// (older .1 is overwritten on the next rotation).

func TestRotatingLog_AccumulatesUnderCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stderr.log")
	rl, err := openRotatingLog(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	if _, err := rl.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Fatalf("content mismatch: %q", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf(".1 should not exist yet: %v", err)
	}
}

func TestRotatingLog_RotatesOnOverflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stderr.log")
	rl, err := openRotatingLog(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	// First write fits (12 bytes < 16).
	if _, err := rl.Write([]byte("aaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	// Second write would push total to 24 > 16 → rotate, then write.
	if _, err := rl.Write([]byte("bbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("expected rotated backup at %s.1: %v", path, err)
	}
	if !bytes.Equal(cur, []byte("bbbbbbbbbbbb")) {
		t.Fatalf("current file wrong after rotate: %q", cur)
	}
	if !bytes.Equal(old, []byte("aaaaaaaaaaaa")) {
		t.Fatalf(".1 file wrong: %q", old)
	}
}

func TestRotatingLog_KeepsOnlyOneBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stderr.log")
	rl, err := openRotatingLog(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	// Force three rotations; only the most recent pre-rotation content
	// should survive in .1.
	writes := [][]byte{
		[]byte("AAAAAAA"), // 7 bytes
		[]byte("BBBBBBB"), // rotate → .1 = AAAA...
		[]byte("CCCCCCC"), // rotate → .1 = BBBB...
		[]byte("DDDDDDD"), // rotate → .1 = CCCC...
	}
	for _, w := range writes {
		if _, err := rl.Write(w); err != nil {
			t.Fatal(err)
		}
	}

	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if string(cur) != "DDDDDDD" {
		t.Fatalf("current = %q, want DDDDDDD", cur)
	}
	if string(old) != "CCCCCCC" {
		t.Fatalf(".1 = %q, want CCCCCCC (only one backup kept)", old)
	}
	// No deeper archive should exist.
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf(".2 should not exist: %v", err)
	}
}

func TestRotatingLog_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	rl, err := openRotatingLog(filepath.Join(dir, "x.log"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rl.Close(); err != nil {
		t.Fatalf("double-close: %v", err)
	}
	if _, err := rl.Write([]byte("x")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("write after close: expected closed error, got %v", err)
	}
}

func TestLogMaxBytes_EnvOverride(t *testing.T) {
	t.Setenv(logMaxBytesEnv, "4096")
	if got := logMaxBytes(); got != 4096 {
		t.Fatalf("env override: got %d, want 4096", got)
	}
	t.Setenv(logMaxBytesEnv, "not-a-number")
	if got := logMaxBytes(); got != defaultLogMaxBytes {
		t.Fatalf("bad env falls back to default: got %d, want %d", got, defaultLogMaxBytes)
	}
	t.Setenv(logMaxBytesEnv, "")
	if got := logMaxBytes(); got != defaultLogMaxBytes {
		t.Fatalf("empty env = default: got %d", got)
	}
}

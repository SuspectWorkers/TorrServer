package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitSharedLogPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "combined.log")

	Init(path, path)
	defer Close()

	if logFile == nil {
		t.Fatal("expected logFile to be set")
	}
	if webLogFile != logFile {
		t.Fatal("expected webLogFile to share logFile when paths are equal")
	}
	if webLog == nil {
		t.Fatal("expected webLog to be set")
	}
}

func TestInitSeparateLogPaths(t *testing.T) {
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "server.log")
	webPath := filepath.Join(dir, "web.log")

	Init(serverPath, webPath)
	defer Close()

	if logFile == nil || webLogFile == nil {
		t.Fatal("expected both log files to be set")
	}
	if logFile == webLogFile {
		t.Fatal("expected separate file handles for different paths")
	}
}

func TestCloseSharedLogPathOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "combined.log")

	Init(path, path)
	Close()

	if logFile != nil || webLogFile != nil || webLog != nil {
		t.Fatal("expected log handles to be cleared after Close")
	}
}

func TestLogRotationAndReadLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")

	SetMaxLogBytes(512)
	t.Cleanup(func() { SetMaxLogBytes(DefaultMaxLogBytes) })

	ringBuf.Reset()
	Init(path, "")
	defer Close()

	line := strings.Repeat("X", 120)
	for i := 0; i < 6; i++ {
		TLogln("entry", i, line)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated backup %s.1 to exist: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected active log file %s to exist: %v", path, err)
	}
	if fi.Size() > 512 {
		t.Fatalf("active log file size %d exceeds max 512 bytes", fi.Size())
	}

	got := string(ReadLogs(4096))
	if !strings.Contains(got, "entry 5") {
		t.Fatalf("expected ReadLogs to include latest entry, got %q", got)
	}
	if !strings.Contains(got, "entry 0") {
		t.Fatalf("expected ReadLogs to include earlier entry from backup file, got %q", got)
	}
}

func TestRingBufferCapturesWithoutLogFile(t *testing.T) {
	ringBuf.Reset()
	Init("", "")
	defer Close()

	TLogln("ring-only-probe-line")
	got := string(ReadLogs(64 * 1024))
	if !strings.Contains(got, "ring-only-probe-line") {
		t.Fatalf("expected ReadLogs to return ring buffer line, got %q", got)
	}
}

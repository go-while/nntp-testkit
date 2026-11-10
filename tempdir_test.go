package testkit

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShortTempDir(t *testing.T) {
	dir := ShortTempDir(t)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("ShortTempDir %q: %v", dir, err)
	}
	if len(dir) > 40 {
		t.Errorf("ShortTempDir %q has %d bytes, want a short path", dir, len(dir))
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", st.Mode().Perm())
	}
	if other := ShortTempDir(t); other == dir {
		t.Errorf("two calls returned the same directory %q", dir)
	}
}

func TestShortTempDirLongTMPDIR(t *testing.T) {
	if st, err := os.Stat("/tmp"); err != nil || !st.IsDir() {
		t.Skip("no /tmp")
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 80))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	// A name as long as a table-driven subtest's: t.TempDir() would put it under TMPDIR.
	t.Run(strings.Repeat("LongSubtestName_", 6), func(t *testing.T) {
		dir := ShortTempDir(t)
		if strings.HasPrefix(dir, long) || filepath.Dir(dir) != "/tmp" {
			t.Fatalf("ShortTempDir %q: want a directory directly under /tmp, not under the long TMPDIR", dir)
		}
		if len(dir) > 40 {
			t.Errorf("ShortTempDir %q has %d bytes", dir, len(dir))
		}
		if td := t.TempDir(); SocketPathFits(filepath.Join(td, strings.Repeat("d", 40), "s.sock")) {
			t.Errorf("t.TempDir() %q is no longer than the socket limit with this TMPDIR; the premise of the test is gone", td)
		}
	})
}

func TestShortTempDirCleanup(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = ShortTempDir(t)
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists after the test: %v", dir, err)
	}
}

func TestShortTempDirNameFollowsTest(t *testing.T) {
	var a, b string
	t.Run("a", func(t *testing.T) { a = filepath.Base(ShortTempDir(t)) })
	t.Run("b", func(t *testing.T) { b = filepath.Base(ShortTempDir(t)) })
	pa, pb, _ := strings.Cut(a, "-")
	qa, qb, _ := strings.Cut(b, "-")
	if pa == qa || pb == "" || qb == "" {
		t.Errorf("names %q and %q: want a hash prefix that differs per test, then a random suffix", a, b)
	}
}

func TestShortTempDirSocketInDeepSubdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir := ShortTempDir(t)
	sub := filepath.Join(dir, "run", "innd", "sockets", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A socket path of exactly the limit.
	path := filepath.Join(sub, strings.Repeat("s", MaxSocketPath-len(sub)-1-len(".sock"))+".sock")
	if len(path) != MaxSocketPath {
		t.Fatalf("test setup: path has %d bytes, want %d", len(path), MaxSocketPath)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind %s (%d bytes, limit %d): %v", path, len(path), MaxSocketPath, err)
	}
	_ = l.Close()
	// One byte more than the limit cannot be bound; the helper's limit is the real one.
	over := filepath.Join(sub, strings.Repeat("o", MaxSocketPath+1-len(sub)-1))
	if SocketPathFits(over) || len(over) != MaxSocketPath+1 {
		t.Fatalf("test setup: %d bytes", len(over))
	}
	if l, err := net.Listen("unix", over); err == nil {
		_ = l.Close()
		t.Errorf("bound a %d-byte socket path, MaxSocketPath = %d is too low", len(over), MaxSocketPath)
	}
}

func TestSocketPathFits(t *testing.T) {
	if !SocketPathFits(strings.Repeat("a", MaxSocketPath)) || SocketPathFits(strings.Repeat("a", MaxSocketPath+1)) {
		t.Errorf("SocketPathFits is not exact at MaxSocketPath = %d", MaxSocketPath)
	}
	if MaxSocketPath != 107 && MaxSocketPath != 103 {
		t.Errorf("MaxSocketPath = %d", MaxSocketPath)
	}
}

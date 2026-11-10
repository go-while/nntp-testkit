package testkit

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

// shortBase is the longest os.TempDir() ShortTempDir uses as it is; a longer one (macOS:
// /var/folders/xx/yyyy.../T, about 50 bytes) makes it use /tmp.
const shortBase = 32

// SocketPathFits reports whether path can be the address of a unix-domain socket on this
// system: at most MaxSocketPath bytes.
func SocketPathFits(path string) bool { return len(path) <= MaxSocketPath }

// ShortTempDir returns a new empty directory (mode 0700) with a short path, removed with its
// contents by tb.Cleanup, for files whose full path must stay short: above all unix-domain
// sockets. The address of a socket (sockaddr_un.sun_path) holds at most 107 bytes on Linux and
// 103 on macOS and the BSDs (MaxSocketPath); a longer path makes bind or connect fail with
// "invalid argument" or "file name too long", often with a message that does not name the length.
//
// tb.TempDir() is os.TempDir() + "/" + the test name (cut and sanitised) + a random number +
// "/001". On macOS os.TempDir() alone is about 50 bytes, and a long test name, or a subtest name
// built from a table entry, takes the rest: five virtual machines of one project failed this way
// with a long TMPDIR. ShortTempDir takes os.TempDir() when it is at most 32 bytes and /tmp
// otherwise (os.TempDir() when /tmp is not a directory), and names the directory "nt" + 6 hex
// digits of a hash of tb.Name() + "-" + random digits: about 25 bytes in all, whatever the test is
// called, and still traceable to its test when someone looks at /tmp. That leaves about 75 bytes
// for the names below it; assert with SocketPathFits.
//
// Call it before starting servers that use the directory: cleanups run last-in first-out, and
// the directory must be removed after the servers have stopped.
func ShortTempDir(tb testing.TB) string {
	tb.Helper()
	base := os.TempDir()
	if len(base) > shortBase {
		if st, err := os.Stat("/tmp"); err == nil && st.IsDir() {
			base = "/tmp"
		}
	}
	sum := sha256.Sum256([]byte(tb.Name()))
	dir, err := os.MkdirTemp(base, fmt.Sprintf("nt%x-", sum[:3]))
	if err != nil {
		tb.Fatalf("testkit.ShortTempDir: %v", err)
	}
	tb.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			tb.Errorf("testkit.ShortTempDir: remove %s: %v", dir, err)
		}
	})
	return dir
}

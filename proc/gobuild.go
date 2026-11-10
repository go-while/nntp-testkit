package proc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// GoBuildFloor is the least time GoBuild allows for one build, whatever NNTP_TEST_DEADLINE says:
// a cold build cache compiles the standard library and every dependency, which takes seconds to
// minutes. The timeout is the larger of this and testkit.Deadline().
const GoBuildFloor = 5 * time.Minute

// buildEntry is one cached build. mu serialises the builds of a key; bin or compileErr is set
// once a build has ended for good. A transient failure (timeout, I/O error) sets neither, so the
// next caller builds again.
// goBuildTimeout is the time one build may take; a variable for the tests.
var goBuildTimeout = func() time.Duration { return max(GoBuildFloor, testkit.Deadline()) }

type buildEntry struct {
	mu         sync.Mutex
	bin        string
	compileErr error
}

var builds struct {
	mu      sync.Mutex
	entries map[string]*buildEntry
	taken   map[string]string // binary path -> key
	tmp     string            // the shared dir used when out is ""
}

// GoBuild builds the Go main package pkg ("./cmd/server", or an import path) with "go build"
// and returns the path of the binary, for a Go server run under StartTCP or StartStdio.
//
// out is the directory to put the binary in, created when missing; "" = a directory under
// os.TempDir() shared by the process, which CleanBuilds removes (call it from TestMain after
// m.Run, or it stays behind). The file is named after the last element of pkg (the base name
// of the working directory for "."), with a hash suffix when another build already took the
// name in that directory. ldflags are joined with spaces into one -ldflags argument; quote an
// element that contains a space the way cmd/go expects ('-X main.v="a b"').
//
// Builds are cached per process by (out, the package, ldflags, and GOOS, GOARCH, GOFLAGS and
// CGO_ENABLED of the environment): tests that ask for the same one share a single build, also
// when they run in parallel. A relative pkg ("./cmd/srv", ".") counts as the directory it names
// from the working directory, an import path as that path in the module of the working
// directory, so the same name from another directory is another build. A compiler failure
// (go build exits non-zero) is remembered, so the others fail at once; a timeout or an I/O
// error (disk full) is not, the next caller builds again. Two test processes (packages run by
// "go test ./..." in parallel) that build into the same explicit out race for the file: give
// each its own directory, or use "" (a directory per process).
//
// The go tool runs in the caller's directory with the environment of the test, under TryRun:
// a timeout (the larger of GoBuildFloor and testkit.Deadline()) kills the build with its
// children. A failed build fails tb with the compiler output.
func GoBuild(tb testing.TB, out, pkg string, ldflags ...string) string {
	tb.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		tb.Fatalf("proc.GoBuild: go tool not on PATH: %v", err)
	}
	if out == "" {
		out = sharedBuildDir(tb)
	} else if out, err = filepath.Abs(out); err != nil {
		tb.Fatalf("proc.GoBuild: %v", err)
	}
	flags := strings.Join(ldflags, " ")
	wd, err := os.Getwd()
	if err != nil {
		tb.Fatalf("proc.GoBuild: %v", err)
	}
	where := wd + "\x00" + pkg // an import path resolves in the module of the working directory
	if pkg == "." || pkg == ".." || strings.HasPrefix(pkg, "./") || strings.HasPrefix(pkg, "../") {
		where = filepath.Join(wd, filepath.FromSlash(pkg))
	}
	parts := []string{out, where, flags}
	for _, v := range []string{"GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED"} {
		parts = append(parts, os.Getenv(v))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00\x01")))
	key := hex.EncodeToString(sum[:])

	builds.mu.Lock()
	if builds.entries == nil {
		builds.entries = map[string]*buildEntry{}
		builds.taken = map[string]string{}
	}
	e := builds.entries[key]
	if e == nil {
		e = &buildEntry{}
		builds.entries[key] = e
	}
	builds.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.compileErr != nil {
		tb.Fatalf("%v", e.compileErr)
	}
	if e.bin != "" {
		return e.bin
	}
	bin, compile, err := doBuild(goTool, out, pkg, flags, key)
	switch {
	case err == nil:
		e.bin = bin
		return bin
	case compile:
		e.compileErr = err
	}
	tb.Fatalf("%v", err)
	return ""
}

// doBuild runs the build; compile is true when the go tool ran and failed (the failure is the
// code's), false for anything that may pass on a retry.
func doBuild(goTool, out, pkg, flags, key string) (bin string, compile bool, err error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", false, fmt.Errorf("proc.GoBuild: %w", err)
	}
	name := path.Base(filepath.ToSlash(pkg))
	if name == "." || name == ".." || name == "/" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false, fmt.Errorf("proc.GoBuild: %w", err)
		}
		name = filepath.Base(wd)
	}
	bin = filepath.Join(out, name)
	builds.mu.Lock()
	if other, ok := builds.taken[bin]; ok && other != key {
		bin = filepath.Join(out, name+"-"+key[:8])
	}
	builds.taken[bin] = key
	builds.mu.Unlock()

	args := []string{"build", "-o", bin}
	if flags != "" {
		args = append(args, "-ldflags="+flags)
	}
	args = append(args, pkg)
	text, code, err := TryRun(RunOptions{Path: goTool, Args: args, Timeout: goBuildTimeout()})
	if err != nil {
		return "", false, fmt.Errorf("proc.GoBuild %s: %w", pkg, err)
	}
	if code != 0 {
		return "", true, fmt.Errorf("proc.GoBuild: go %s: exit %d\n%s", strings.Join(args, " "), code, text)
	}
	return bin, false, nil
}

// sharedBuildDir returns the process-wide build directory, creating it on first use.
func sharedBuildDir(tb testing.TB) string {
	tb.Helper()
	builds.mu.Lock()
	defer builds.mu.Unlock()
	if builds.tmp == "" {
		dir, err := os.MkdirTemp("", "nntp-testkit-build-")
		if err != nil {
			tb.Fatalf("proc.GoBuild: %v", err)
		}
		builds.tmp = dir
	}
	return builds.tmp
}

// CleanBuilds removes the shared directory GoBuild used for builds with out == "" and forgets
// the cached builds; it does nothing when there is none. Call it from TestMain after
// m.Run: func TestMain(m *testing.M) { code := m.Run(); proc.CleanBuilds(); os.Exit(code) }.
func CleanBuilds() {
	builds.mu.Lock()
	defer builds.mu.Unlock()
	if builds.tmp == "" {
		return
	}
	_ = os.RemoveAll(builds.tmp)
	builds.entries = nil
	builds.taken = nil
	builds.tmp = ""
}

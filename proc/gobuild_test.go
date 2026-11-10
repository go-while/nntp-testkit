package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGoBuildCachesPerPackageAndFlags(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH")
	}
	out := t.TempDir()
	bin := GoBuild(t, out, "./testdata/echo-server")
	if filepath.Dir(bin) != out || filepath.Base(bin) != "echo-server" {
		t.Errorf("binary %q, want %s/echo-server", bin, out)
	}
	st1, err := os.Stat(bin)
	if err != nil || st1.Mode()&0o111 == 0 {
		t.Fatalf("binary: %v %v", st1, err)
	}
	// Same key: same path, no rebuild (the file is not touched), also from many goroutines.
	var wg sync.WaitGroup
	paths := make([]string, 8)
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i] = GoBuild(t, out, "./testdata/echo-server")
		}()
	}
	wg.Wait()
	for _, p := range paths {
		if p != bin {
			t.Errorf("cached build returned %q, want %q", p, bin)
		}
	}
	st2, _ := os.Stat(bin)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Errorf("binary rebuilt: mtime %v -> %v", st1.ModTime(), st2.ModTime())
	}
	// Other ldflags: a build of its own, next to the first one.
	other := GoBuild(t, out, "./testdata/echo-server", "-s", "-w")
	if other == bin || filepath.Dir(other) != out {
		t.Errorf("ldflags build %q, want another file in %s", other, out)
	}
	if st, err := os.Stat(other); err != nil || st.Size() >= st1.Size() {
		t.Errorf("stripped binary: %v (%v), want smaller than %d", st, err, st1.Size())
	}
	if again := GoBuild(t, out, "./testdata/echo-server", "-s", "-w"); again != other {
		t.Errorf("second ldflags build %q, want %q", again, other)
	}
	// The built server runs.
	addr := freeAddr(t)
	StartTCP(t, Options{Path: bin, Args: []string{"-listen", addr}}, addr)
}

func TestGoBuildSharedDirAndClean(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH")
	}
	a := GoBuild(t, "", "./testdata/echo-server")
	b := GoBuild(t, "", "./testdata/echo-server")
	if a != b {
		t.Errorf("shared builds differ: %q %q", a, b)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal(err)
	}
	CleanBuilds()
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("%s survives CleanBuilds: %v", a, err)
	}
	CleanBuilds() // nothing to do
	c := GoBuild(t, "", "./testdata/echo-server")
	t.Cleanup(CleanBuilds)
	if _, err := os.Stat(c); err != nil {
		t.Errorf("rebuild after CleanBuilds: %v", err)
	}
}

func TestGoBuildFailureFailsTB(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH")
	}
	out := t.TempDir()
	for range 2 { // the failure is cached, the second call fails too
		ftb := &fakeTB{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			GoBuild(ftb, out, "./testdata/no-such-package")
		}()
		<-done
		if !ftb.Failed() || len(ftb.fatals) != 1 || !strings.Contains(ftb.fatals[0], "no-such-package") {
			t.Errorf("fatals %q", ftb.fatals)
		}
	}
}

// writeModule makes a module with a main package that prints msg.
func writeModule(t *testing.T, msg string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{
		"go.mod":  "module example.com/srv\n\ngo 1.21\n",
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"" + msg + "\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGoBuildSameRelativePackageFromOtherDirectories(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH")
	}
	out := t.TempDir()
	var bins []string
	for _, msg := range []string{"first", "second"} {
		t.Chdir(writeModule(t, msg))
		bin := GoBuild(t, out, ".")
		bins = append(bins, bin)
		got, code := Run(t, RunOptions{Path: bin})
		if code != 0 || strings.TrimSpace(got) != msg {
			t.Errorf("binary of module %q printed %q (code %d)", msg, got, code)
		}
	}
	if bins[0] == bins[1] {
		t.Errorf("two directories share one binary %q", bins[0])
	}
	if again := GoBuild(t, out, "."); again != bins[1] {
		t.Errorf("same directory again: %q, want %q", again, bins[1])
	}
}

func TestGoBuildTransientFailureNotCached(t *testing.T) {
	fake := t.TempDir()
	script := func(body string) {
		if err := os.WriteFile(filepath.Join(fake, "go"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := goBuildTimeout
	goBuildTimeout = func() time.Duration { return 300 * time.Millisecond }
	t.Cleanup(func() { goBuildTimeout = old })
	out := t.TempDir()
	try := func() string {
		ftb := &fakeTB{TB: t}
		done := make(chan struct{})
		go func() { defer close(done); GoBuild(ftb, out, "./fake-pkg") }()
		<-done
		if len(ftb.fatals) != 1 {
			t.Fatalf("fatals %q, want one", ftb.fatals)
		}
		return ftb.fatals[0]
	}
	script("exec sleep 30")
	if got := try(); !strings.Contains(got, "did not finish within") || !strings.Contains(got, ErrTimeout.Error()) {
		t.Errorf("timeout: %q", got)
	}
	script("echo boom; exit 1") // not cached: the next call builds again, and this failure is the compiler's
	if got := try(); !strings.Contains(got, "boom") {
		t.Errorf("after a timeout the build was not retried: %q", got)
	}
	script("echo other; exit 1") // cached: the first compiler failure is repeated
	if got := try(); !strings.Contains(got, "boom") {
		t.Errorf("compiler failure not cached: %q", got)
	}
}

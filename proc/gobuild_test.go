package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

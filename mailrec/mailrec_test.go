package mailrec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testMsg = "From: poster@example.invalid\r\n" +
	"To: mod@example.invalid\r\n" +
	"Subject: a long\r\n\tfolded subject\r\n" +
	"X-Note: has\r\n\tTo: inside a continuation\r\n" +
	"\r\n" +
	"body line\r\n"

func TestMailParse(t *testing.T) {
	m := Mail{Argv: []string{"/usr/sbin/sendmail", "-oi", "-f", "news@example.invalid", "-odq",
		"a@example.invalid", "-F", "News", "--", "-odd@example.invalid"}, Msg: []byte(testMsg)}
	if got, want := m.Recipients(), []string{"a@example.invalid", "-odd@example.invalid"}; !slices.Equal(got, want) {
		t.Errorf("Recipients = %q, want %q", got, want)
	}
	if got := m.From(); got != "news@example.invalid" {
		t.Errorf("From = %q", got)
	}
	if m.HeaderMode() {
		t.Error("HeaderMode true without -t")
	}
	if !(Mail{Argv: []string{"sendmail", "-t", "-i"}}).HeaderMode() {
		t.Error("HeaderMode false with -t")
	}
	if got := (Mail{Argv: []string{"sendmail", "-fbounce@example.invalid", "x@example.invalid"}}).From(); got != "bounce@example.invalid" {
		t.Errorf("From with attached -f = %q", got)
	}
	for name, want := range map[string]string{
		"subject": "a long folded subject",
		"To":      "mod@example.invalid",
		"x-note":  "has To: inside a continuation",
		"Cc":      "",
	} {
		if got := m.Header(name); got != want {
			t.Errorf("Header(%q) = %q, want %q", name, got, want)
		}
	}
	if got := m.Body(); got != "body line\r\n" {
		t.Errorf("Body = %q", got)
	}
}

func TestRecordRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mail")
	if m, err := Read(dir); err != nil || m != nil {
		t.Fatalf("Read of a missing dir = %v, %v; want no mail", m, err)
	}
	for i, to := range []string{"a@example.invalid", "b@example.invalid"} {
		n, err := Record(dir, []string{"sendmail", "-i", to}, strings.NewReader(testMsg))
		if err != nil || n != i+1 {
			t.Fatalf("Record = %d, %v; want %d", n, err, i+1)
		}
	}
	// a mail still being written (no done file) is not returned
	if err := os.Mkdir(filepath.Join(dir, "3"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m[0].N != 1 || m[1].Recipients()[0] != "b@example.invalid" || string(m[0].Msg) != testMsg {
		t.Fatalf("Read = %+v", m)
	}
}

func TestMainStatus(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	if got := Main([]string{"sendmail"}, strings.NewReader("x"), getenv, ""); got != 0 {
		t.Errorf("no record dir: status %d, want 0", got)
	}
	if got := Main([]string{"sendmail"}, strings.NewReader("x"), getenv, dir); got != 0 {
		t.Errorf("built-in dir: status %d, want 0", got)
	}
	env[EnvExit] = "69"
	if got := Main([]string{"sendmail"}, strings.NewReader("x"), getenv, dir); got != 69 {
		t.Errorf("%s=69: status %d", EnvExit, got)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Main([]string{"sendmail"}, strings.NewReader("x"), getenv, dir); got != 3 {
		t.Errorf("exit file 3 (wins over %s): status %d", EnvExit, got)
	}
	other := filepath.Join(t.TempDir(), "other")
	env[EnvDir] = other
	delete(env, EnvExit)
	if got := Main([]string{"sendmail"}, strings.NewReader("y"), getenv, dir); got != 0 {
		t.Errorf("%s set: status %d", EnvDir, got)
	}
	if m, _ := Read(other); len(m) != 1 || string(m[0].Msg) != "y" {
		t.Errorf("%s did not win over the built-in dir: %+v", EnvDir, m)
	}
	if m, _ := Read(dir); len(m) != 3 {
		t.Errorf("built-in dir has %d mails, want 3 (failed mails are recorded too)", len(m))
	}
}

// TestNew builds the command and runs it as a server would, with an empty environment.
func TestNew(t *testing.T) {
	r := New(t)
	run := func(args ...string) error {
		cmd := exec.Command(r.Path, args...)
		cmd.Env = []string{}
		cmd.Stdin = strings.NewReader(testMsg)
		return cmd.Run()
	}
	r.ExpectNone(t)
	if err := run("-i", "mod@example.invalid"); err != nil {
		t.Fatalf("run: %v", err)
	}
	r.SetExit(t, 75)
	err := run("-t")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 75 {
		t.Fatalf("after SetExit(75): %v", err)
	}
	m := r.Wait(t, 2)
	if got := m[0].Recipients(); !slices.Equal(got, []string{"mod@example.invalid"}) || !m[1].HeaderMode() {
		t.Errorf("recorded %+v", m)
	}
	if m[0].Header("Subject") != "a long folded subject" {
		t.Errorf("Subject = %q", m[0].Header("Subject"))
	}
	if !strings.HasPrefix(r.Env(), EnvDir+"=") {
		t.Errorf("Env = %q", r.Env())
	}
}

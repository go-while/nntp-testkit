// Package mailrec records mail instead of sending it, for tests of servers that hand mail to
// sendmail (a moderated group's submissions, control-message reports, newgroup notices). The
// command cmd/fakesendmail stands in for sendmail and writes each mail into a record directory;
// New builds it for one test, and Read and Wait return what it recorded. Only the standard
// library is used.
//
// A record directory holds one subdirectory per mail, numbered from 1 in arrival order:
// N/argv (the arguments, one per line, argv[0] first), N/msg (the message as read from stdin)
// and N/done (empty, written last; Read skips a mail without it). The exit status is the number
// in the file "exit" of the record directory, else EnvExit, else 0; the mail is recorded either
// way, as a real MTA may have queued it before failing. Diablo's e2e tests use the same layout
// (tests/e2e/ci/fake-sendmail).
package mailrec

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// EnvDir names the record directory for cmd/fakesendmail. It wins over the directory New
// builds in, so one installed binary can serve several tests where the server passes its
// environment on to the mailer.
const EnvDir = "NNTP_TEST_MAIL_DIR"

// EnvExit is the exit status of cmd/fakesendmail when the record directory has no "exit" file.
const EnvExit = "NNTP_TEST_MAIL_EXIT"

// Package is the import path of the stand-in sendmail that New builds.
const Package = "github.com/go-while/nntp-testkit/cmd/fakesendmail"

// Mail is one recorded message.
type Mail struct {
	N    int      // its number in the record directory, from 1
	Argv []string // the arguments, argv[0] first
	Msg  []byte   // the message as given on stdin
}

// valueOpts are the sendmail options that take a value, attached ("-faddr") or as the next
// argument ("-f addr").
const valueOpts = "BFLNORVXfhpr"

// parse splits Argv into options and recipients: "--" ends the options, and every argument
// that is neither an option nor an option's value is a recipient.
func (m Mail) parse() (opts map[byte]string, rcpts []string) {
	opts = map[byte]string{}
	args := m.Argv
	if len(args) > 0 {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rcpts = append(rcpts, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			rcpts = append(rcpts, a)
			continue
		}
		o := a[1]
		v := a[2:]
		if strings.IndexByte(valueOpts, o) >= 0 && v == "" && i+1 < len(args) {
			i++
			v = args[i]
		}
		opts[o] = v
	}
	return opts, rcpts
}

// Recipients returns the addresses given as arguments (with -t the MTA also reads To:, Cc: and
// Bcc:; see HeaderMode).
func (m Mail) Recipients() []string {
	_, r := m.parse()
	return r
}

// From returns the envelope sender given with -f (or -r), or "".
func (m Mail) From() string {
	o, _ := m.parse()
	if v, ok := o['f']; ok {
		return v
	}
	return o['r']
}

// HeaderMode reports whether -t was given: the MTA takes recipients from the headers.
func (m Mail) HeaderMode() bool {
	o, _ := m.parse()
	_, ok := o['t']
	return ok
}

// Header returns the first header field called name (case-insensitive), unfolded, or "".
func (m Mail) Header(name string) string {
	var cur string
	var val []string
	sc := bufio.NewScanner(bytes.NewReader(m.Msg))
	sc.Buffer(make([]byte, 64*1024), len(m.Msg)+1)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if l == "" {
			break
		}
		if l[0] == ' ' || l[0] == '\t' {
			if cur != "" {
				val = append(val, strings.TrimSpace(l))
			}
			continue
		}
		if cur != "" {
			break
		}
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			cur = k
			val = []string{strings.TrimSpace(v)}
		}
	}
	return strings.Join(val, " ")
}

// Body returns the message after the first empty line, or "" when there is none.
func (m Mail) Body() string {
	s := string(m.Msg)
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if i := strings.Index(s, sep); i >= 0 {
			return s[i+len(sep):]
		}
	}
	return ""
}

// Record writes one mail into dir as the next number, done last. It creates dir if needed.
func Record(dir string, argv []string, msg io.Reader) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 1
	for {
		err := os.Mkdir(filepath.Join(dir, strconv.Itoa(n)), 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return 0, err
		}
		n++
	}
	d := filepath.Join(dir, strconv.Itoa(n))
	var a strings.Builder
	for _, s := range argv {
		a.WriteString(s)
		a.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(d, "argv"), []byte(a.String()), 0o644); err != nil {
		return n, err
	}
	f, err := os.Create(filepath.Join(d, "msg"))
	if err != nil {
		return n, err
	}
	if _, err := io.Copy(f, msg); err != nil {
		_ = f.Close()
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	return n, os.WriteFile(filepath.Join(d, "done"), nil, 0o644)
}

// Read returns the finished mails in dir, in number order. A missing dir is no mail.
func Read(dir string) ([]Mail, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mails []Mail
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(d, "done")); err != nil {
			continue
		}
		a, err := os.ReadFile(filepath.Join(d, "argv"))
		if err != nil {
			return nil, err
		}
		msg, err := os.ReadFile(filepath.Join(d, "msg"))
		if err != nil {
			return nil, err
		}
		mails = append(mails, Mail{N: n, Argv: strings.Split(strings.TrimSuffix(string(a), "\n"), "\n"), Msg: msg})
	}
	sort.Slice(mails, func(i, j int) bool { return mails[i].N < mails[j].N })
	return mails, nil
}

// Main is cmd/fakesendmail: record stdin as a mail into getenv(EnvDir), else into dir (the
// directory built in), and return the exit status. Without either directory the message is read
// and dropped (status 0). A failure to record is status 75 (EX_TEMPFAIL).
func Main(argv []string, stdin io.Reader, getenv func(string) string, dir string) int {
	if d := getenv(EnvDir); d != "" {
		dir = d
	}
	if dir == "" {
		_, _ = io.Copy(io.Discard, stdin)
		return 0
	}
	if _, err := Record(dir, argv, stdin); err != nil {
		fmt.Fprintln(os.Stderr, "fakesendmail:", err)
		return 75
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			return n
		}
	}
	if n, err := strconv.Atoi(getenv(EnvExit)); err == nil {
		return n
	}
	return 0
}

// Recorder is a stand-in sendmail built for one test.
type Recorder struct {
	Path string // the executable: give it to the server as its sendmail or mailer
	Dir  string // the record directory built into it
}

// New builds cmd/fakesendmail into tb.TempDir() with a fresh record directory built in, so it
// records even when the server clears the environment of the programs it runs. It needs the go
// command (on PATH or in $GOROOT/bin) and fails tb without it.
func New(tb testing.TB) *Recorder {
	tb.Helper()
	tmp := tb.TempDir()
	r := &Recorder{Path: filepath.Join(tmp, "sendmail"), Dir: filepath.Join(tmp, "mail")}
	if err := os.Mkdir(r.Dir, 0o755); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	gocmd, err := exec.LookPath("go")
	if err != nil {
		if root := os.Getenv("GOROOT"); root != "" {
			gocmd = filepath.Join(root, "bin", "go")
		}
	}
	if gocmd == "" {
		tb.Fatalf("mailrec: no go command to build %s", Package)
	}
	// -ldflags is split at spaces; quotes group but take no escapes
	v := "main.recordDir=" + r.Dir
	switch {
	case !strings.ContainsAny(v, " \t'\""):
	case !strings.Contains(v, `"`):
		v = `"` + v + `"`
	case !strings.Contains(v, "'"):
		v = "'" + v + "'"
	default:
		tb.Fatalf("mailrec: cannot pass the directory %q to go build", r.Dir)
	}
	x := "-X " + v
	cmd := exec.Command(gocmd, "build", "-o", r.Path, "-ldflags", x, Package)
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("mailrec: go build %s: %v\n%s", Package, err, out)
	}
	return r
}

// Env returns "NNTP_TEST_MAIL_DIR=<Dir>", for a server's environment.
func (r *Recorder) Env() string { return EnvDir + "=" + r.Dir }

// SetExit makes the stand-in exit with code from now on (0 again for success).
func (r *Recorder) SetExit(tb testing.TB, code int) {
	tb.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, "exit"), []byte(strconv.Itoa(code)+"\n"), 0o644); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
}

// Mails returns the mails recorded so far.
func (r *Recorder) Mails(tb testing.TB) []Mail {
	tb.Helper()
	m, err := Read(r.Dir)
	if err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	return m
}

// Wait waits until at least n mails are recorded (bounded by testkit.Deadline()) and returns
// them all.
func (r *Recorder) Wait(tb testing.TB, n int) []Mail {
	tb.Helper()
	var m []Mail
	testkit.Eventually(tb, 0, fmt.Sprintf("%d recorded mail(s) in %s", n, r.Dir), func() bool {
		m, _ = Read(r.Dir)
		return len(m) >= n
	})
	return m
}

// settle is how long ExpectNone watches for a late mail.
const settle = 200 * time.Millisecond

// ExpectNone fails tb if a mail is recorded now or within a short while: for checking that a
// server did not send one.
func (r *Recorder) ExpectNone(tb testing.TB) {
	tb.Helper()
	time.Sleep(settle)
	if m := r.Mails(tb); len(m) > 0 {
		tb.Errorf("mailrec: %d mail(s) recorded, want none; first argv %q", len(m), m[0].Argv)
	}
}

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
//
// The message is recorded exactly as read until end of input: no -oi emulation (a line with a
// lone "." does not end it) and no dot-unstuffing. A test that cares whether the server passed
// -oi checks Argv.
//
// The record directory may be written by a server that runs as another user than the test (INN
// or Diablo started as news): New and NewFromBinary make it world-writable under a directory
// others can traverse, and Record leaves each mail readable and removable by the test.
package mailrec

import (
	"bufio"
	"bytes"
	"context"
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

// EnvDir names the record directory for a cmd/fakesendmail that has none built in (one
// installed binary serving several tests, where the server passes its environment on to the
// mailer). A directory built in by New, or named by MarkerFile, wins over it, so a stale value
// in the server's environment cannot redirect a Recorder's mail.
const EnvDir = "NNTP_TEST_MAIL_DIR"

// MarkerFile is the file next to the cmd/fakesendmail executable that names its record
// directory (the path, one line). NewFromBinary writes it beside its copy of the binary.
const MarkerFile = "fakesendmail.dir"

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

// Body returns the message after the first empty line (CRLF or LF line ends), or "" when there
// is none. A message that starts with an empty line is all body.
func (m Mail) Body() string {
	s := string(m.Msg)
	for _, lead := range []string{"\r\n", "\n"} {
		if strings.HasPrefix(s, lead) {
			return s[len(lead):]
		}
	}
	best, end := -1, 0
	for _, sep := range []string{"\r\n\r\n", "\n\n", "\n\r\n"} {
		if i := strings.Index(s, sep); i >= 0 && (best < 0 || i < best) {
			best, end = i, i+len(sep)
		}
	}
	if best < 0 {
		return ""
	}
	return s[end:]
}

// Record writes one mail into dir as the next number, done last. It creates dir if needed. The
// mail's directory and files are made writable for all (whatever the umask), so a test running
// as another user than the mailing server can read and remove them.
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
	if err := os.Chmod(d, 0o777); err != nil {
		return n, err
	}
	write := func(name string, r io.Reader) error {
		p := filepath.Join(d, name)
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Chmod(p, 0o666)
	}
	var a strings.Builder
	for _, s := range argv {
		a.WriteString(s)
		a.WriteByte('\n')
	}
	if err := write("argv", strings.NewReader(a.String())); err != nil {
		return n, err
	}
	if err := write("msg", msg); err != nil {
		return n, err
	}
	return n, write("done", strings.NewReader(""))
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

// Main is cmd/fakesendmail: record stdin as a mail into dir (the directory built in or named by
// MarkerFile), else into getenv(EnvDir), and return the exit status. Without either directory
// the message is read and dropped (status 0). A failure to record is status 75 (EX_TEMPFAIL).
func Main(argv []string, stdin io.Reader, getenv func(string) string, dir string) int {
	if dir == "" {
		dir = getenv(EnvDir)
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

// buildTimeout bounds the go build in New; a cold build cache takes several seconds.
const buildTimeout = 5 * time.Minute

// newRecorder makes a fresh directory for one test: traversable by all (0711, so a server
// running as another user can exec the binary in it) and removed when the test ends, with the
// record directory "mail" in it writable by all.
func newRecorder(tb testing.TB) *Recorder {
	tb.Helper()
	tmp, err := os.MkdirTemp("", "mailrec-")
	if err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(tmp) })
	r := &Recorder{Path: filepath.Join(tmp, "sendmail"), Dir: filepath.Join(tmp, "mail")}
	if err := os.Chmod(tmp, 0o711); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	if err := os.Mkdir(r.Dir, 0o777); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	if err := os.Chmod(r.Dir, 0o777); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	return r
}

// New builds cmd/fakesendmail for one test, with a fresh record directory built in, so it
// records even when the server clears the environment of the programs it runs. It needs the go
// command (on PATH or in $GOROOT/bin) and the kit's module source (a consumer that vendors its
// dependencies has no cmd/fakesendmail; use NewFromBinary there), and fails tb without them.
// The build takes about a second with a warm build cache.
func New(tb testing.TB) *Recorder {
	tb.Helper()
	r := newRecorder(tb)
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
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gocmd, "build", "-o", r.Path, "-ldflags", x, Package)
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("mailrec: go build %s: %v\n%s", Package, err, out)
	}
	return r
}

// NewFromBinary is New with a cmd/fakesendmail built beforehand (go build -o bin
// github.com/go-while/nntp-testkit/cmd/fakesendmail, once per CI run or host): it copies bin
// into a fresh directory with MarkerFile beside it, so no go command is needed at test time.
func NewFromBinary(tb testing.TB, bin string) *Recorder {
	tb.Helper()
	r := newRecorder(tb)
	src, err := os.Open(bin)
	if err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(r.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		tb.Fatalf("mailrec: copy %s: %v", bin, err)
	}
	if err := dst.Close(); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	if err := os.Chmod(r.Path, 0o755); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.Path), MarkerFile), []byte(r.Dir+"\n"), 0o644); err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	return r
}

// DirFromMarker returns the record directory named by MarkerFile next to the executable exe,
// or "" when there is no such file.
func DirFromMarker(exe string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), MarkerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
}

// Env returns "NNTP_TEST_MAIL_DIR=<Dir>", for a server's environment (only needed for a
// fakesendmail that has no directory of its own).
func (r *Recorder) Env() string { return EnvDir + "=" + r.Dir }

// SetExit makes the stand-in exit with code from now on (0 again for success). The file is
// replaced atomically, so a mailer running at that moment sees the old or the new code.
func (r *Recorder) SetExit(tb testing.TB, code int) {
	tb.Helper()
	f, err := os.CreateTemp(r.Dir, ".exit-")
	if err != nil {
		tb.Fatalf("mailrec: %v", err)
	}
	_, werr := f.WriteString(strconv.Itoa(code) + "\n")
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(f.Name(), filepath.Join(r.Dir, "exit"))
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		tb.Fatalf("mailrec: %v", werr)
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

// Wait waits until at least n mails are finished (bounded by testkit.Deadline()) and returns
// all finished ones. Numbers follow the order in which the mailers started, so with mailers
// running at the same time the n returned need not be numbers 1 to n.
func (r *Recorder) Wait(tb testing.TB, n int) []Mail {
	tb.Helper()
	var m []Mail
	testkit.WaitFor(tb, 0, fmt.Sprintf("%d recorded mail(s) in %s", n, r.Dir), func() bool {
		var err error
		m, err = Read(r.Dir)
		return err == nil && len(m) >= n
	})
	return m
}

// started counts the mails begun in dir, finished or not.
func started(dir string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
			n++
		}
	}
	return n
}

// settle is how long ExpectNone watches for a late mail.
const settle = 200 * time.Millisecond

// ExpectNone fails tb if a mail is begun now or within a short while (settle, 200ms): for
// checking that a server did not send one. A mail still being written counts.
func (r *Recorder) ExpectNone(tb testing.TB) {
	tb.Helper()
	time.Sleep(settle)
	if n := started(r.Dir); n > 0 {
		var first []string
		if m := r.Mails(tb); len(m) > 0 {
			first = m[0].Argv
		}
		tb.Errorf("mailrec: %d mail(s) recorded, want none; first finished argv %q", n, first)
	}
}

package fakeserver

import (
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// DefaultGreeting is the greeting written when Options.Greeting is empty; a Store server with
// StoreOpts.ReaderOnly greets with DefaultGreetingNoPost instead (RFC 3977 5.1.1: 201 when
// posting is prohibited).
const DefaultGreeting = "200 fakeserver ready"

// DefaultGreetingNoPost is the greeting of a read-only Store server.
const DefaultGreetingNoPost = "201 fakeserver ready (no posting)"

// Options configures a Server.
//
// Precedence: RawStall wins over everything (no TLS, no greeting, no Handler/Store/Script).
// Otherwise the greeting is written unless NoGreeting; then Handler runs when set (with
// NoGreeting it may write its own, possibly delayed, greeting), else Store when set, else Script.
// With NoGreeting and no Handler neither Store nor Script runs: lines are only recorded.
type Options struct {
	Addr       string        // listen address, default "127.0.0.1:0"
	Greeting   string        // default DefaultGreeting (DefaultGreetingNoPost for a read-only Store)
	NoGreeting bool          // no greeting; without Handler: never write (greeting stall), reads are still recorded
	RawStall   bool          // accept TCP, never write (TLS-handshake / SOCKS stall); bytes read are discarded
	TLS        *tls.Config   // non-nil: TLS server on every accepted conn (cert from TLSConfig)
	Script     []Step        // run sequentially on every connection after the greeting
	Handler    func(*Conn)   // replaces Script and Store when set; the conn is closed when it returns
	Store      *Store        // stateful mode: the store handler when Handler is nil (see storeHandler)
	StoreOpts  StoreOptions  // options of the store handler
	Faults     Faults        // fault hooks of Store and Script mode; copied by New/Start
	Deadline   time.Duration // hang guard; 0 = testkit.Deadline()
}

// Options.Deadline bounds exactly: the polls of WaitOpen (d is raised to it) and WaitLines, the
// wait of Close for the connection goroutines, and every server-side sleep (Step.Delay,
// Faults.Delay, PeerOptions.StallModeStream are capped at it). Server-side reads have no
// deadline: a connection is held until the client closes it or Close.

// Step is one scripted exchange.
//
// A client line that does not match the current step's Expect gets
// "500 fakeserver: unexpected <line>", is recorded in Mismatches() and the same step waits for
// the next line. After the last step the connection keeps reading and recording lines (no
// replies) until EOF/error, so Open() drops only when the client closes. Hang steps also keep
// reading (after their Delay and Reply, when set).
type Step struct {
	Expect     string        // regexp, anchored ^…$, matched against the client line; "" matches any line
	ReadDot    bool          // after the match, read a dot-terminated block (POST/TAKETHIS payload)
	Delay      time.Duration // before replying; capped at Options.Deadline
	Reply      []string      // lines written with CRLF; multi-line replies include their "." line
	Hang       bool          // after the match (and ReadDot, Delay, Reply), never reply again; held until Close
	CloseAfter bool          // close the connection after the reply
}

// StoreOptions configures the store handler (Options.StoreOpts, StoreHandler).
type StoreOptions struct {
	User, Pass  string   // non-empty: AUTHINFO USER/PASS required (POST before auth → 480)
	ReaderOnly  bool     // no posting
	OverviewFmt []string // LIST OVERVIEW.FMT lines; empty = the handler's default
}

// Faults are fault hooks keyed by upper-cased command name ("ARTICLE"). They apply in Store
// mode and in Script mode only: an external Handler cannot apply them. New and Start copy the
// maps, so changing Options.Faults afterwards has no effect (and does not race); do not mutate
// the maps while New or Start runs.
//
// In Script mode: Delay and Override apply to the Reply of a matched step (not to the
// "500 fakeserver: unexpected" answer of a mismatch); Delay runs after the step's own Delay;
// CloseAfter counts every client line read with ReadLine on the connection (mismatches and the
// lines after the last step included) and closes after the reply to the N-th; DropInMultiline
// counts every reply line, the status line included, and closes after N of them when the reply
// is longer (0 = close before writing anything). A Hang step holds its connection: the faults
// no longer apply after its reply.
//
// The faults apply in this order: Delay; OverrideLine, else Override; LongLine;
// DropInMultiline; then the reply is written, through Drip when set, and Endless goes on from
// there. OverrideLine is keyed by the whole client line, normalised as the command upper-cased
// and the arguments as sent, joined by single spaces ("ARTICLE 2", "OVER 11-20",
// "ARTICLE <a@b>"); New and Start normalise its keys the same way. Drip and Endless are bounded
// by Options.Deadline like every server-side wait: after it the connection is closed.
type Faults struct {
	Delay           map[string]time.Duration // command → sleep before its reply
	CloseAfter      int                      // >0: close the connection after this many commands
	Override        map[string][]string      // command → reply lines written instead of the normal reply
	DropInMultiline map[string]int           // command → close the connection after this many lines of its multi-line reply

	// OverrideLine is Override for one client line: "ARTICLE 2" → {"430 No such article"}
	// answers only that article, e.g. one 430 in a pipeline. It wins over Override.
	OverrideLine map[string][]string
	// Drip writes the reply one byte at a time with this pause between bytes: a slow server
	// that never goes quiet for long, which a per-read timeout does not catch.
	Drip map[string]time.Duration
	// Endless writes the reply without its "." terminator and then filler lines ("<n> endless")
	// until the client closes, the server is closed or the Deadline passes: a never-ending
	// response. The connection is closed after it.
	Endless map[string]bool
	// LongLine inserts one line of that many "x" bytes after the status line of the reply: an
	// overlong line, in a multi-line reply as part of its block.
	LongLine map[string]int
}

// clone returns a deep copy of f (nil maps stay nil).
func (f Faults) clone() Faults {
	out := Faults{CloseAfter: f.CloseAfter}
	if f.OverrideLine != nil {
		out.OverrideLine = make(map[string][]string, len(f.OverrideLine))
		for k, v := range f.OverrideLine {
			out.OverrideLine[lineKey(k)] = append([]string(nil), v...)
		}
	}
	if f.Drip != nil {
		out.Drip = make(map[string]time.Duration, len(f.Drip))
		for k, v := range f.Drip {
			out.Drip[k] = v
		}
	}
	if f.Endless != nil {
		out.Endless = make(map[string]bool, len(f.Endless))
		for k, v := range f.Endless {
			out.Endless[k] = v
		}
	}
	if f.LongLine != nil {
		out.LongLine = make(map[string]int, len(f.LongLine))
		for k, v := range f.LongLine {
			out.LongLine[k] = v
		}
	}
	if f.Delay != nil {
		out.Delay = make(map[string]time.Duration, len(f.Delay))
		for k, v := range f.Delay {
			out.Delay[k] = v
		}
	}
	if f.Override != nil {
		out.Override = make(map[string][]string, len(f.Override))
		for k, v := range f.Override {
			out.Override[k] = append([]string(nil), v...)
		}
	}
	if f.DropInMultiline != nil {
		out.DropInMultiline = make(map[string]int, len(f.DropInMultiline))
		for k, v := range f.DropInMultiline {
			out.DropInMultiline[k] = v
		}
	}
	return out
}

// storeHandler builds the handler of Store mode (Options.Store set, Handler nil) once per
// Server. It is set by storehandler.go; while it is nil, Store mode only records lines (like
// NoGreeting without Handler, but after the greeting). The handler applies the server's Faults
// through Conn.writeReply and Conn.closeDue.
var storeHandler func(*Store, StoreOptions) func(*Conn)

// Server is a fake NNTP server: scripted, handler-driven or stateful. Create it with New or Start.
type Server struct {
	host string // listen IP, e.g. "127.0.0.1"
	port int

	tb        testing.TB // nil for Start
	o         Options
	deadline  time.Duration
	re        []*regexp.Regexp // compiled Script expectations; nil = any line
	handler   func(*Conn)      // Options.Handler, or the store handler
	ln        net.Listener
	done      chan struct{} // closed by Close
	wg        sync.WaitGroup
	closeOnce sync.Once

	mu         sync.Mutex
	closed     bool
	conns      map[net.Conn]struct{}
	lines      []string
	articles   [][]string
	mismatches []string
	counts     map[string]int
	accepted   int
	open       int
}

// New listens on o.Addr, serves every connection in its own goroutine and registers
// tb.Cleanup(s.Close). A listen error or a bad Step.Expect fails tb.
func New(tb testing.TB, o Options) *Server {
	tb.Helper()
	s, err := start(tb, o)
	if err != nil {
		tb.Fatalf("fakeserver: %v", err)
	}
	tb.Cleanup(s.Close)
	return s
}

// Start is New without a testing dependency (for a CLI): the caller calls Close.
func Start(o Options) (*Server, error) {
	return start(nil, o)
}

// start listens and starts the accept loop; tb (nil for Start) is kept for Close's report.
func start(tb testing.TB, o Options) (*Server, error) {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	if o.Greeting == "" {
		o.Greeting = DefaultGreeting
		if o.Store != nil && o.Handler == nil && o.StoreOpts.ReaderOnly {
			o.Greeting = DefaultGreetingNoPost
		}
	}
	s := &Server{
		tb:       tb,
		o:        o,
		deadline: o.Deadline,
		done:     make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
		counts:   make(map[string]int),
	}
	if s.deadline <= 0 {
		s.deadline = testkit.Deadline()
	}
	s.o.Faults = o.Faults.clone()
	for i, st := range o.Script {
		if st.Expect == "" {
			s.re = append(s.re, nil)
			continue
		}
		re, err := regexp.Compile("^(?:" + st.Expect + ")$")
		if err != nil {
			return nil, fmt.Errorf("step %d: bad Expect %q: %w", i, st.Expect, err)
		}
		s.re = append(s.re, re)
	}
	switch {
	case o.Handler != nil:
		s.handler = o.Handler
	case o.Store != nil && storeHandler != nil:
		s.handler = storeHandler(o.Store, o.StoreOpts)
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", o.Addr, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("unexpected listener address %v", ln.Addr())
	}
	s.ln = ln
	s.host = tcp.IP.String()
	s.port = tcp.Port
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return // listener closed by Close
			default:
			}
			time.Sleep(10 * time.Millisecond) // transient accept error: retry
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.accepted++
		s.open++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) serve(raw net.Conn) {
	defer s.wg.Done()
	c := &Conn{s: s, raw: raw, conn: raw}
	defer c.finish()
	if s.o.RawStall {
		buf := make([]byte, 4096)
		for {
			if _, err := raw.Read(buf); err != nil {
				return
			}
		}
	}
	if s.o.TLS != nil {
		c.conn = tls.Server(raw, s.o.TLS)
	}
	c.r = newReader(c.conn)
	if !s.o.NoGreeting {
		if err := c.WriteLines(s.o.Greeting); err != nil {
			return
		}
	} else if s.o.Handler == nil {
		c.drain()
		return
	}
	switch {
	case s.handler != nil:
		s.handler(c)
	case s.o.Store != nil:
		c.drain() // no store handler linked in: record only
	default:
		s.runScript(c)
	}
}

func (s *Server) runScript(c *Conn) {
	for i := 0; i < len(s.o.Script); {
		st := s.o.Script[i]
		line, err := c.ReadLine()
		if err != nil {
			return
		}
		if re := s.re[i]; re != nil && !re.MatchString(line) {
			s.mu.Lock()
			s.mismatches = append(s.mismatches, line)
			s.mu.Unlock()
			if err := c.WriteLines("500 fakeserver: unexpected " + line); err != nil {
				return
			}
			if c.closeDue() {
				return
			}
			continue
		}
		if st.ReadDot {
			if _, err := c.ReadDotBlock(); err != nil {
				return
			}
		}
		if st.Delay > 0 && !c.sleep(st.Delay) {
			return
		}
		if !c.writeReply(line, st.Reply...) {
			return
		}
		if st.Hang {
			c.drain()
			return
		}
		if st.CloseAfter || c.closeDue() {
			return
		}
		i++
	}
	for {
		if _, err := c.ReadLine(); err != nil {
			return
		}
		if c.closeDue() {
			return
		}
	}
}

// Addr returns the listen address as "host:port".
func (s *Server) Addr() string { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }

// Host returns the listen IP, e.g. "127.0.0.1".
func (s *Server) Host() string { return s.host }

// Port returns the listen port.
func (s *Server) Port() int { return s.port }

// Lines returns every client line (CRLF stripped) in arrival order, all connections.
// Payload lines read by ReadDotBlock are not included (see Articles).
func (s *Server) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// Articles returns every dot block as raw wire lines (still dot-stuffed, without the "."
// terminator), in arrival order.
func (s *Server) Articles() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.articles))
	for i, a := range s.articles {
		out[i] = append([]string{}, a...)
	}
	return out
}

// Mismatches returns the client lines that did not match the current step's Expect.
func (s *Server) Mismatches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mismatches...)
}

// Accepted returns the number of accepted connections.
func (s *Server) Accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// Open returns the connections whose server-side read has not seen EOF/error (and that the
// server has not closed itself).
func (s *Server) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// Count returns how many client lines read through Conn.ReadLine started with cmd (compared
// upper-cased), in all modes and on all connections.
func (s *Server) Count(cmd string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[command(cmd)]
}

// Counts returns a copy of every command count: upper-cased first word of each client line
// read through Conn.ReadLine ("" for a blank line) → number of lines.
func (s *Server) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// WaitOpen polls Open every 10 ms until it equals n; tb.Fatalf after d. d is raised to the
// server's Deadline: every caller waits for an expected state, so d is only a hang guard.
func (s *Server) WaitOpen(tb testing.TB, n int, d time.Duration) {
	tb.Helper()
	d = max(d, s.deadline)
	deadline := time.Now().Add(d)
	for {
		got := s.Open()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("fakeserver: Open() = %d, want %d within %v", got, n, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitLines polls Lines every 10 ms until it equals want (nil and empty are equal); tb.Fatalf
// after the server's Deadline.
func (s *Server) WaitLines(tb testing.TB, want []string) {
	tb.Helper()
	deadline := time.Now().Add(s.deadline)
	for {
		got := s.Lines()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("fakeserver: Lines() = %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close closes the listener and every connection and waits up to the server's Deadline for the
// accept and connection goroutines (a Handler that blocks elsewhere is reported to the tb of
// New; Start servers just return). Safe to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		_ = s.ln.Close()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		waited := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(waited)
		}()
		tm := time.NewTimer(s.deadline)
		defer tm.Stop()
		select {
		case <-waited:
		case <-tm.C:
			if s.tb != nil {
				s.tb.Errorf("fakeserver: connection goroutines still running %v after Close", s.deadline)
			}
		}
	})
}

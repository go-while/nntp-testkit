// Package fakeserver is a fake NNTP server for tests: scripted (a fixed sequence of expected
// client lines and replies), handler-driven (your func gets the connection) or stateful (a Store
// of groups and articles served by the built-in RFC 3977 handler), with fault hooks and command
// counters. It listens on 127.0.0.1:0 unless given another address and never contacts any host.
//
// Contract (plan nntp-testkit). Implemented in server.go (Options, Step, StoreOptions, Faults,
// Server), conn.go, tls.go, peer.go, store.go and storehandler.go:
//
//	type Options struct {
//		Addr       string        // listen address, default "127.0.0.1:0"
//		Greeting   string        // default "200 fakeserver ready"
//		NoGreeting bool          // no greeting; without Handler: never write (greeting stall), reads are still recorded
//		RawStall   bool          // accept TCP, never write (TLS-handshake / SOCKS stall); bytes read are discarded
//		TLS        *tls.Config   // non-nil: TLS server on every accepted conn (cert from TLSConfig)
//		Script     []Step        // run sequentially on every connection after the greeting
//		Handler    func(*Conn)   // replaces Script and Store when set; the conn is closed when it returns
//		Store      *Store        // stateful mode: StoreHandler(Store, StoreOptions) when Handler is nil
//		StoreOpts  StoreOptions
//		Faults     Faults        // apply in Script and Store mode (copied at start); an external Handler cannot use them
//		Deadline   time.Duration // bounds WaitOpen/WaitLines/Close and caps Step.Delay, Faults.Delay, StallModeStream; server-side reads have no deadline; 0 = testkit.Deadline()
//		MaxConns         int    // >0: one more simultaneous connection gets MaxConnsGreeting and is closed (counts in Accepted, never in Open)
//		MaxConnsGreeting string // default "502 Too many connections"
//	}
//	// Precedence: RawStall wins over everything. Otherwise the greeting is written unless
//	// NoGreeting; then Handler when set, else Store when set, else Script (with NoGreeting and
//	// no Handler the Script/Store is not run: lines are only recorded).
//
//	type Step struct { Expect string; ReadDot bool; Delay time.Duration; Reply []string; Hang, CloseAfter bool }
//	// Semantics as go-pugleaf's cliFakeStep: Expect is an anchored regexp ("" = any line); a
//	// non-matching line gets "500 fakeserver: unexpected <line>" and is recorded in Mismatches();
//	// after the last step the connection keeps reading and recording until EOF.
//
//	type Server struct { ... }
//	func New(tb testing.TB, o Options) *Server       // listens, serves, tb.Cleanup(Close); fails tb on listen error
//	func Start(o Options) (*Server, error)          // the same without testing (for a CLI)
//	func (s *Server) Addr() string                   // "host:port"
//	func (s *Server) Host() string
//	func (s *Server) Port() int
//	func (s *Server) Lines() []string                // every client line in order, all connections
//	func (s *Server) Articles() [][]string           // every dot block read (ReadDot steps, POST/TAKETHIS payloads), raw wire lines
//	func (s *Server) Mismatches() []string
//	func (s *Server) Accepted() int                  // connections accepted so far
//	func (s *Server) Open() int                      // connections not yet gone
//	func (s *Server) WaitOpen(tb testing.TB, n int, d time.Duration) // polls until Open()==n; fails tb after d
//	func (s *Server) WaitLines(tb testing.TB, want []string)         // polls until Lines()==want (within Deadline); fails tb
//	func (s *Server) Count(cmd string) int           // commands seen: upper-cased first word of every client line
//	func (s *Server) Counts() map[string]int
//	func (s *Server) Close()                         // closes listener and every connection, waits for the goroutines
//
//	type Conn struct { ... }                         // one server-side connection
//	func (c *Conn) ReadLine() (string, error)        // CRLF/LF stripped; records the line and counts the command
//	func (c *Conn) WriteLines(lines ...string) error // each + CRLF
//	func (c *Conn) ReadDotBlock() ([]string, error)  // raw wire lines up to "."; recorded in Articles()
//	func (c *Conn) Raw() net.Conn                    // the TLS conn when TLS, else the TCP conn
//	func (c *Conn) Server() *Server
//	func (c *Conn) ID() int                          // accept order from 1 (rejected connections count too)
//
//	func TLSConfig(tb testing.TB) (server *tls.Config, roots *x509.CertPool) // self-signed ECDSA cert for 127.0.0.1
//
//	type PeerOptions struct { User, Pass string; Have map[string]bool; Defer431Once map[string]bool; StallModeStream time.Duration; OnArticle func(msgID string, wire []string) }
//	func PeerHandler(o *PeerOptions) func(*Conn)      // streaming peer: CAPABILITIES, MODE STREAM/READER, AUTHINFO, CHECK, TAKETHIS, QUIT; else 500
//
// Store (stateful mode; all methods are safe for concurrent use):
//
//	type Article struct { Number int64; MessageID string; Headers []string; Body []string; Date time.Time }
//	type Group struct { Name, Description string; Low, High, Count int64; Posting bool; Created time.Time }
//	type Location struct { Group string; Number int64 }
//	type Store struct { ... }
//	func NewStore() *Store
//	func (st *Store) AddGroup(name, description string)                 // idempotent; Posting true; Created = now when created
//	func (st *Store) AddGroupAt(name, description string, created time.Time) // AddGroup with an explicit Created (also re-dates an existing group when non-zero)
//	func (st *Store) SetPosting(group string, posting bool) bool        // LIST ACTIVE flag; false for an unknown group
//	func (st *Store) AddArticle(group string, a Article) int64          // number = a.Number when > 0 (must be free, else panic) else High+1; Date zero → now; a missing Message-ID (field and header) is generated as <n.group@fakeserver.invalid> and indexed; a field that differs from the header panics; returns the number; a group that does not exist is created
//	func (st *Store) Article(group string, n int64) (Article, bool)
//	func (st *Store) ByMessageID(id string) (Article, []Location, bool) // first article stored under id, every location
//	func (st *Store) Group(name string) (Group, bool)
//	func (st *Store) Groups() []Group                                   // sorted by name
//	func (st *Store) Numbers(group string) []int64                      // ascending, for LISTGROUP
//	func (st *Store) Overview(group string, from, to int64) []Article   // ascending numbers within [from,to]; to<=0 = High
//	func (st *Store) AddPosted(a Article)                               // articles accepted by POST (also filed into their Newsgroups by the handler)
//	func (st *Store) Posted() []Article
//	func (st *Store) Next(group string, n int64) (int64, bool)          // next existing number above n
//	func (st *Store) Last(group string, n int64) (int64, bool)          // previous existing number below n
//	func (st *Store) RemoveArticle(group string, n int64) bool          // expiry/cancel: High is kept (not reused), Low/Count follow, the location leaves ByMessageID; an emptied group has Low High+1
//	func (st *Store) Expire(group string, below int64) int              // RemoveArticle of every number below below
//	func (st *Store) Cancel(id string) int                              // RemoveArticle of every location of id
//	func (st *Store) HighestExisting(conn int, g Group) Group           // for StoreOptions.GroupNumbers: High = highest existing number (Low-1 when empty)
//	func (a Article) Header(name string) string                         // first matching header value, case-insensitive, unfolded; "" when absent
//	func (a Article) WithDefaults(group string) Article                 // copy with Message-ID, Newsgroups (when group != "") and Date headers added when missing
//	const DateLayout = "Mon, 02 Jan 2006 15:04:05 -0700"
//
//	type StoreOptions struct {
//		User, Pass        string
//		ReaderOnly        bool
//		OverviewFmt       []string
//		GroupNumbers      func(conn int, g Group) Group // numbers GROUP, LISTGROUP and LIST ACTIVE report, per connection (lagging frontends, "211 0 0 0")
//		ListgroupRange    string   // "" RFC 3977; "reject": 501 for the range form; "ignore": the whole group
//		NoOver, NoXover   bool     // that command is unknown (500); NoOver drops "OVER MSGID" from CAPABILITIES
//		EmptyOverReply    []string // instead of 423 for a range without articles
//		MaxOverRange      int64    // >0: cap of one OVER/XOVER range (open ranges end at High)
//		OverRangeTooLarge []string // reply to a larger range; nil = only its first MaxOverRange numbers
//	}
//	func StoreHandler(st *Store, o StoreOptions) func(*Conn)
//	// Serves CAPABILITIES (VERSION 2, READER, LIST ACTIVE NEWSGROUPS OVERVIEW.FMT, OVER MSGID, POST
//	// unless ReaderOnly, AUTHINFO USER while auth is required, IMPLEMENTATION fakeserver), MODE
//	// READER (200/201), HELP, LIST [ACTIVE [wildmat]|NEWSGROUPS [wildmat]|OVERVIEW.FMT], GROUP,
//	// LISTGROUP, ARTICLE/HEAD/BODY/STAT (by number, message-id or current; 412/420/423/430),
//	// XOVER/OVER (range "n", "n-", "n-m" or message-id; 423 for an empty range), NEXT/LAST
//	// (421/422), NEWGROUPS (Group.Created; RFC 3977 7.3.2 century rule), DATE, POST (440 when
//	// ReaderOnly; 340 then 240/441: Newsgroups, Subject and From required, Message-ID must be
//	// <...> and unused, generated as <posted-N@fakeserver.invalid> when missing; stored with
//	// AddPosted and AddArticle per group), AUTHINFO USER/PASS (381/281/481/482; USER alone
//	// answers 281/481 when Pass is empty; 502 when not configured or done), QUIT (205).
//	// When User or Pass is set every other command answers 480 until authenticated; any 480
//	// the handler writes to an authenticated connection (also one from Faults.Override) ends
//	// its login, so AUTHINFO is taken again (a login that timed out). Unknown
//	// command 500, bad arguments 501. Served headers: the stored Headers plus Message-ID,
//	// Newsgroups (every group the article is filed in) and Date when missing. Faults apply to
//	// every reply (for POST: to the 340).
//
//	type Faults struct {
//		Delay           map[string]time.Duration // command → sleep before its reply
//		CloseAfter      int                      // >0: close the connection after this many commands
//		Override        map[string][]string      // command → reply lines written instead of the normal reply
//		DropInMultiline map[string]int           // command → close the connection after this many lines of its multi-line reply
//		OverrideLine    map[string][]string      // client line ("ARTICLE 2") → reply lines; wins over Override
//		Drip            map[string]time.Duration // command → write its reply one byte at a time, this pause between bytes
//		Endless         map[string]bool          // command → reply without terminator, then "<n> endless" lines until close or Deadline
//		LongLine        map[string]int           // command → one line of N "x" bytes after the status line
//		LineDelay       map[string]time.Duration // command → write its reply one line at a time, this pause between lines
//		OverrideFrom    map[string]int           // command → its Override/OverrideLine apply only from its N-th use on a connection
//		OverrideTimes   map[string]int           // command → its Override/OverrideLine apply at most N times per connection
//	}
//	// Keys are upper-cased command names ("ARTICLE"), OverrideLine's the whole client line
//	// normalised (command upper-cased, arguments joined by single spaces); the hooks apply in
//	// Store mode and in Script mode (there: only to a matched step's reply), in this order:
//	// Delay, OverrideLine else Override, LongLine, DropInMultiline, then the write (Drip),
//	// then Endless (LineDelay pauses between the lines of the write). DropInMultiline counts the status line; 0 closes before writing anything.
//	// CloseAfter counts every client line, mismatches included. Drip and Endless stop at the
//	// Deadline and close the connection.
//	const DefaultGreeting = "200 fakeserver ready"            // DefaultGreetingNoPost ("201 ...") for a read-only Store server
package fakeserver

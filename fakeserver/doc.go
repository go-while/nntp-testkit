// Package fakeserver is a fake NNTP server for tests: scripted (a fixed sequence of expected
// client lines and replies), handler-driven (your func gets the connection) or stateful (a Store
// of groups and articles served by the built-in RFC 3977 handler), with fault hooks and command
// counters. It listens on 127.0.0.1:0 unless given another address and never contacts any host.
//
// Contract (plan nntp-testkit). Implemented in server.go, conn.go, tls.go, peer.go (wave 2 core),
// store.go (wave 1), storehandler.go and faults.go (wave 2):
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
//		Faults     Faults        // see faults.go; apply to Store and Script modes
//		Deadline   time.Duration // hang guard of waits and raw reads; 0 = testkit.Deadline()
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
//
//	func TLSConfig(tb testing.TB) (server *tls.Config, roots *x509.CertPool) // self-signed ECDSA cert for 127.0.0.1
//
//	type PeerOptions struct { User, Pass string; Have map[string]bool; Defer431Once map[string]bool; StallModeStream time.Duration; OnArticle func(msgID string, wire []string) }
//	func PeerHandler(o *PeerOptions) func(*Conn)      // streaming peer: CAPABILITIES, MODE STREAM/READER, AUTHINFO, CHECK, TAKETHIS, QUIT; else 500
//
// Store (stateful mode; all methods are safe for concurrent use):
//
//	type Article struct { Number int64; MessageID string; Headers []string; Body []string; Date time.Time }
//	type Group struct { Name, Description string; Low, High, Count int64; Posting bool }
//	type Location struct { Group string; Number int64 }
//	type Store struct { ... }
//	func NewStore() *Store
//	func (st *Store) AddGroup(name, description string)                 // idempotent; Posting true
//	func (st *Store) AddArticle(group string, a Article) int64          // number = a.Number when > 0 (must be free) else High+1; Date zero → now; returns the number; a group that does not exist is created
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
//
//	type StoreOptions struct { User, Pass string; ReaderOnly bool; OverviewFmt []string }
//	func StoreHandler(st *Store, o StoreOptions) func(*Conn)
//	// Serves CAPABILITIES, MODE READER, LIST [ACTIVE [wildmat]|NEWSGROUPS|OVERVIEW.FMT], GROUP,
//	// LISTGROUP, ARTICLE/HEAD/BODY/STAT (by number, message-id or current), XOVER/OVER (range or
//	// message-id), NEXT/LAST, NEWGROUPS, DATE, POST (480 before auth when User set; 340/240;
//	// stores with AddPosted and files into the Newsgroups header's groups), AUTHINFO USER/PASS
//	// (381/281/481), QUIT (205). Codes per RFC 3977/4643; unknown command 500; 411/412/420/423/430
//	// where the RFC says so. Headers of a served article: the stored Headers; when none hold
//	// Message-ID/Newsgroups/Date they are added.
//
//	type Faults struct {
//		Delay           map[string]time.Duration // command → sleep before its reply
//		CloseAfter      int                      // >0: close the connection after this many commands
//		Override        map[string][]string      // command → reply lines written instead of the normal reply
//		DropInMultiline map[string]int           // command → close the connection after this many lines of its multi-line reply
//	}
//	// Keys are upper-cased command names ("ARTICLE"); the hooks apply in Store mode and in Script mode.
package fakeserver

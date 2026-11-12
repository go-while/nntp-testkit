// Package fakeserver is a fake NNTP server for tests: scripted (a fixed sequence of expected
// client lines and replies), handler-driven (your func gets the connection) or stateful (a Store
// of groups and articles served by the built-in RFC 3977 handler), with fault hooks and command
// counters. It listens on 127.0.0.1:0 unless given another address and never contacts any host.
//
// Contract (plan nntp-testkit). Implemented in server.go (Options, Step, StoreOptions, Faults,
// Server), conn.go, tls.go, starttls.go, peer.go, store.go and storehandler.go:
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
//		Faults     Faults        // apply in Script and Store mode, to a LineHandler and to PeerHandler (copied at start); another Handler cannot use them
//		Deadline   time.Duration // bounds WaitOpen/WaitLines/Close and caps Step.Delay, Faults.Delay, StallModeStream, GreetingDelay, HoldGreeting; server-side reads have no deadline (a TLS handshake, implicit or STARTTLS, is bounded by it); 0 = testkit.Deadline()
//		MaxConns         int    // >0: one more simultaneous connection gets MaxConnsGreeting and is closed (counts in Accepted, never in Open)
//		MaxConnsGreeting string // default "502 Too many connections"
//	}
//	// Precedence: RawStall wins over everything. Otherwise the greeting is written unless
//	// NoGreeting; then Handler when set, else Store when set, else Script (with NoGreeting and
//	// no Handler the Script/Store is not run: lines are only recorded).
//
//	type Step struct { Expect string; ReadDot bool; Delay time.Duration; Reply []string; Hang, CloseAfter, Compress bool; CompressShape CompressShape }
//	// Semantics as go-pugleaf's cliFakeStep: Expect is an anchored regexp ("" = any line); a
//	// non-matching line gets "500 fakeserver: unexpected <line>" and is recorded in Mismatches();
//	// after the last step the connection keeps reading and recording until EOF. Compress
//	// switches the connection to COMPRESS DEFLATE after the step's reply (Conn.StartCompress),
//	// CompressShape shapes its output stream.
//
//	type Server struct { ... }
//	func New(tb testing.TB, o Options) *Server       // listens, serves, tb.Cleanup(Close); fails tb on listen error
//	func Start(o Options) (*Server, error)          // the same without testing (for a CLI)
//	func (s *Server) Addr() string                   // "host:port"
//	func (s *Server) Host() string
//	func (s *Server) Port() int
//	func NewPeer(tb testing.TB, o Options, po *PeerOptions) *Server // New with Handler = PeerHandler(po) and NoGreeting set when po has HoldGreeting/GreetingDelay; po nil = &PeerOptions{}; an o.Handler already set fails tb
//	func StartPeer(o Options, po *PeerOptions) (*Server, error)      // the same without testing
//	func (s *Server) Lines() []string                // every client line in order, all connections
//	func (s *Server) Articles() [][]string           // every dot block read (ReadDot steps, POST/TAKETHIS/IHAVE payloads), raw wire lines
//	func (s *Server) ArticlesRaw() [][]byte          // the same blocks as the client wrote them: the exact bytes between the command and the "." terminator, stuffed, CRLF intact; article.ParseDotBlock takes an element
//	func (s *Server) Mismatches() []string
//	func (s *Server) Accepted() int                  // connections accepted so far
//	func (s *Server) Open() int                      // connections not yet gone
//	func (s *Server) MaxOpen() int                   // the most connections open at once (rejected ones do not count)
//	func (s *Server) Closed() int                    // connections that ended on the server side (client closed or aborted, or the server dropped it); those that counted in Open and are gone from it, rejected (MaxConns) ones not
//	func (s *Server) WaitOpen(tb testing.TB, n int, d time.Duration) // polls until Open()==n; fails tb after d
//	func (s *Server) WaitClosed(tb testing.TB, n int, d time.Duration) // polls until Closed()>=n; fails tb after d (raised to the Deadline); unlike Open()==0 it cannot pass before the connection was accepted
//	func (s *Server) WaitLines(tb testing.TB, want []string)         // polls until Lines()==want (within Deadline); fails tb
//	func (s *Server) Count(cmd string) int           // commands seen: upper-cased first word of every client line
//	func (s *Server) Counts() map[string]int
//	func (s *Server) Close()                         // closes listener and every connection, waits for the goroutines
//
//	type Conn struct { ... }                         // one server-side connection
//	func (c *Conn) ReadLine() (string, error)        // CRLF/LF stripped; records the line and counts the command
//	func (c *Conn) WriteLines(lines ...string) error // each + CRLF
//	func (c *Conn) ReadDotBlock() ([]string, error)  // raw wire lines up to "."; recorded in Articles() and ArticlesRaw()
//	func (c *Conn) ReadDotBlockRaw() ([]string, []byte, error) // the same, also the exact bytes of the block (not the "." line)
//	func (c *Conn) Raw() net.Conn                    // the TLS conn when TLS, else the TCP conn; after StartCompress the compressing conn on top
//	func (c *Conn) StartCompress() error             // RFC 8054 COMPRESS DEFLATE from now on, both directions; call it after writing the 206; error when already on
//	func (c *Conn) StartCompressShaped(s CompressShape) error // StartCompress with the output stream shaped, see CompressShape below
//	func (c *Conn) StartCompressOutputOnly() error   // broken COMPRESS: deflate only the server's output, read the client's bytes uncompressed
//	func (c *Conn) Compressed() bool
//	func (c *Conn) Server() *Server
//	func (c *Conn) ID() int                          // accept order from 1 (rejected connections count too)
//
//	func TLSConfig(tb testing.TB) (server *tls.Config, roots *x509.CertPool) // self-signed ECDSA cert for 127.0.0.1 (package tlscert: a CA, leaf options, files)
//	func NewTLSConfig() (server *tls.Config, roots *x509.CertPool, err error) // TLSConfig without tb, for a fake server program
//
//	type StartTLSOptions struct {   // StoreOptions.StartTLS, PeerOptions.StartTLS: STARTTLS (RFC 4642); nil = unknown command (500)
//		Config              *tls.Config // server config of the upgraded connection; required
//		Refuse              string      // reply instead of 382 ("502 ...", "580 ..."): a refusal / stripping attacker; the connection stays plain
//		HideCapability      bool        // STARTTLS not listed in CAPABILITIES
//		AuthNeedsTLS        bool        // with User/Pass set: AUTHINFO before TLS 483, commands needing auth 483 (not 480), Store lists "AUTHINFO" without USER
//		CloseAfterHandshake int         // >0: the first N upgrades of the server are closed right after the handshake
//	}
//	func (c *Conn) StartTLS(cfg *tls.Config) error  // TLS server side from now on; call it after writing the 382; error when TLS/compression is on, the handshake fails or plaintext was pipelined after STARTTLS
//	func (c *Conn) TLS() bool                       // implicit (Options.TLS) or upgraded
//	func (c *Conn) TLSState() (tls.ConnectionState, bool)
//	func (s *Server) TLSHandshakes() int            // completed handshakes: implicit TLS and STARTTLS
//	func (s *Server) TLSFailed() int                // failed handshakes
//	func (s *Server) TLSServerNames() []string      // SNI of every ClientHello in order ("" when the client sent none), failed handshakes included
//
//	type PeerOptions struct {
//		User, Pass      string          // non-empty: AUTHINFO required; CHECK/TAKETHIS/IHAVE before auth → 480 (TAKETHIS after reading its article, IHAVE before any 335)
//		Have            map[string]bool // ids the peer holds (CHECK 438, TAKETHIS 439, IHAVE 435); TAKETHIS 239 and IHAVE 235 add to it
//		Defer431Once    map[string]bool // CHECK of these ids → 431 the first time only
//		Defer436Once    map[string]bool // IHAVE of these ids → 436 the first time only, as the initial reply (innd: paused, or the article is arriving elsewhere)
//		Reject          map[string]bool // refused after the transfer, every time: CHECK 238, TAKETHIS 439, IHAVE 335 then 437; not added to Have, no OnArticle
//		NoStream        bool            // innd with streaming off: MODE STREAM 501, CHECK/TAKETHIS unknown (500, TAKETHIS's article lines are read as commands), no STREAMING capability
//		StallModeStream time.Duration   // sleep before the handler's first MODE STREAM reply
//		OnArticle       func(msgID string, wire []string) // before the 239/235 reply
//		OnArticleRaw    func(msgID string, raw []byte)    // right after OnArticle: the article's exact stuffed bytes, no terminator (article.ParseDotBlock)
//		Compress           bool // offer COMPRESS DEFLATE as StoreOptions.Compress; AUTHINFO answers 502 once active
//		CompressOutputOnly bool // offer it, but after the 206 compress only the output (Conn.StartCompressOutputOnly)
//		CompressShape      CompressShape // shape the compressed output stream, see below
//		HoldGreeting       <-chan struct{} // greeting waits for close/receive; needs Options.NoGreeting (NewPeer sets it); the hold ends at Deadline (60 s default) and then the connection is dropped without a greeting, as on Close; the conn counts as open
//		GreetingDelay      time.Duration   // sleep before the greeting (after HoldGreeting), capped at Deadline; needs Options.NoGreeting (NewPeer sets it)
//		BeforeReply        func(c *Conn, cmd, line, status string) // before every reply (IHAVE: 335 and the final one); may block, e.g. c.Wait(ch)
//		CapabilitiesReply  []string        // non-nil: the whole answer to CAPABILITIES, e.g. {"500 What?"} (Diablo's feeder)
//		ModeStreamReply    []string        // non-nil: the answer to MODE STREAM, e.g. {"203 StreamOK."} or {"501 ..."}; wins over NoStream's 501
//		DropAfterChecks    int             // >0: per connection, the CHECK after this many gets DropReply and the connection closes
//		DropReply          string          // default "400 Service temporarily unavailable"
//		SilentAfterModeStream bool         // answer MODE STREAM, then read and record but never answer, until client close, Close or Deadline
//		StartTLS           *StartTLSOptions // offer STARTTLS as the Store handler does
//	}
//	func (c *Conn) Wait(ch <-chan struct{}) bool // true: ch fired; false: Deadline passed or server closed
//	func (c *Conn) Sleep(d time.Duration) bool   // false: server closed; d capped at Deadline
//	func LineHandler(f func(c *Conn, line string) (reply []string, closeConn bool)) func(*Conn) // f answers each client line, in any order; Faults apply as in Script mode
//	func PeerHandler(o *PeerOptions) func(*Conn)      // transit peer: CAPABILITIES (VERSION 2, IHAVE, READER, STREAMING, COMPRESS DEFLATE), MODE STREAM/READER, AUTHINFO, CHECK, TAKETHIS, IHAVE, COMPRESS, QUIT; else 500
//	// Faults apply to PeerHandler as to a LineHandler (also CloseAfter); IHAVE has two replies,
//	// as POST: the first (335) takes every fault, an overridden first reply that is not a 335
//	// is final and no article is read; the final 235/437 takes only OverrideFinal, DelayFinal,
//	// Split, DropAfterBytes, Trailing and Abort (BeforeReply can hold it too). TAKETHIS has one
//	// reply, which takes all faults. BeforeReply runs before the Faults. HoldGreeting/GreetingDelay: the handler
//	// writes Options.Greeting itself, so Options.NoGreeting must be set: NewPeer/StartPeer
//	// do that, with a manual Handler: PeerHandler(po) the test fails on the first connection.
//	// IHAVE <id>: 480 before auth; 435 for an id not "<...>" (innd; not 501); 435 held; 436
//	// Defer436Once; else 335, the article (dot block), then 437 (Reject, or held meanwhile) or
//	// 235 (added to Have, OnArticle).
//
// Store (stateful mode; all methods are safe for concurrent use):
//
//	type Article struct { Number int64; MessageID string; Headers []string; Body []string; Date, Arrived time.Time } // Arrived: arrival time for NEWNEWS
//	type Group struct { Name, Description string; Low, High, Count int64; Posting bool; Created time.Time }
//	type Location struct { Group string; Number int64 }
//	type Store struct { ... }
//	func NewStore() *Store
//	func (st *Store) AddGroup(name, description string)                 // idempotent; Posting true; Created = now when created
//	func (st *Store) AddGroupAt(name, description string, created time.Time) // AddGroup with an explicit Created (also re-dates an existing group when non-zero)
//	func (st *Store) SetPosting(group string, posting bool) bool        // LIST ACTIVE flag; false for an unknown group
//	func (st *Store) AddArticle(group string, a Article) int64          // number = a.Number when > 0 (must be free, else panic) else High+1; Date and Arrived zero → now; a missing Message-ID (field and header) is generated as <n.group@fakeserver.invalid> and indexed; a field that differs from the header panics; returns the number; a group that does not exist is created
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
//	func (st *Store) AddGenerated(group, description string, gen Generator) // a generated group: articles Low..High made on demand, never stored; Count = High-Low+1
//	func (st *Store) SetGeneratedHigh(group string, high int64) bool    // new articles arrive in a generated group
//	func (st *Store) NumbersIn(group string, from, to int64) []int64    // existing numbers within [from,to]; to<=0 = High; also for generated groups (within the limit)
//	func (st *Store) GenLimit(group string) int64                       // span limit of a generated group, 0 = none / stored group
//	type Generator struct { Low, High int64; Article func(group string, n int64) (Article, bool); Lookup func(id string) (int64, bool); Limit int64 }
//	func GeneratedArticle(group string, n int64) (Article, bool)        // default: <n.group@fakeserver.invalid>, Date 2020-01-01 + n s (a zero Arrived of a generated article = its Date)
//	const DefaultGenLimit = 100000                                      // OVER/XOVER/LISTGROUP spanning more numbers of a generated group, or NEWNEWS matching such a group → 503
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
//		NoOver, NoXover   bool     // that command is unknown (500, not in HELP); NoOver drops "OVER MSGID" from CAPABILITIES
//		NoNewnews         bool     // NEWNEWS is unknown (500), not in CAPABILITIES or HELP (INN's "502 ... disabled": Faults.Override)
//		EmptyOverReply    []string // instead of 423 for a range without articles
//		MaxOverRange      int64    // >0: cap of one OVER/XOVER range (open ranges end at High)
//		OverRangeTooLarge []string // reply to a larger range; nil = only its first MaxOverRange numbers
//		OverviewFields    func(conn int, group string, fields []string) []string // rewrite the fields of every OVER line (overview out of step with the articles)
//		Compress           bool // offer COMPRESS DEFLATE (RFC 8054), see below
//		CompressOutputOnly bool // offer it broken: after the 206 only the output is compressed, the client's input is read uncompressed
//		CompressShape      CompressShape // shape the compressed output stream, see below
//		CompressBeforeAuth bool // with User/Pass: list and take COMPRESS before AUTHINFO (INN's nnrpd); other commands still 480, AUTHINFO 502 once compressed
//		StartTLS           *StartTLSOptions // offer STARTTLS, see below
//	}
//	func StoreHandler(st *Store, o StoreOptions) func(*Conn)
//	// Serves CAPABILITIES (VERSION 2, READER, LIST ACTIVE ACTIVE.TIMES NEWSGROUPS OVERVIEW.FMT HEADERS, NEWNEWS, OVER MSGID, POST
//	// unless ReaderOnly, AUTHINFO USER while auth is required, COMPRESS DEFLATE with Compress
//	// once authenticated (before with CompressBeforeAuth) and while compression is off, AUTHINFO without arguments once compressed, IMPLEMENTATION fakeserver), MODE
//	// READER (200/201), HELP, LIST [ACTIVE [wildmat]|ACTIVE.TIMES [wildmat]|NEWSGROUPS [wildmat]|OVERVIEW.FMT|HEADERS], GROUP,
//	// LISTGROUP, ARTICLE/HEAD/BODY/STAT (by number, message-id or current; 412/420/423/430),
//	// XOVER/OVER (range "n", "n-", "n-m" or message-id; 423 for an empty range), NEXT/LAST
//	// (421/422), NEWGROUPS (Group.Created; RFC 3977 7.3.2 century rule), NEWNEWS wildmat date
//	// time [GMT] (230: the message-ids, each once, of the articles in matching groups whose
//	// Arrived is at or after the time; date and time as NEWGROUPS, always UTC; 503 when a
//	// matching generated group spans more than its limit), DATE, POST (440 when
//	// ReaderOnly; 340 then 240/441: Newsgroups, Subject and From required, Message-ID must be
//	// <...> and unused, generated as <posted-N@fakeserver.invalid> when missing; stored with
//	// AddPosted and AddArticle per group), AUTHINFO USER/PASS (381/281/481/482; USER alone
//	// answers 281/481 when Pass is empty; 502 when not configured, done or compression is active), COMPRESS alg (with
//	// Compress or CompressOutputOnly, else 500; 480 before required auth; 501 missing or
//	// invalid algorithm name, case-sensitive; 502 when active; 503 not DEFLATE; 206 and
//	// compression from the CRLF of the 206 on), QUIT (205).
//	// When User or Pass is set every other command answers 480 until authenticated; any 480
//	// the handler writes to an authenticated connection (also one from Faults.Override) ends
//	// its login, so AUTHINFO is taken again (a login that timed out). Unknown
//	// command 500, bad arguments 501. Served headers: the stored Headers plus Message-ID,
//	// Newsgroups (every group the article is filed in) and Date when missing. Faults apply to
//	// every reply (for POST: every fault to the 340, the final-reply faults to the 240/441, see Faults).
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
//		Split           map[string]Split         // command → write its reply in pieces: Split{At []int (byte offsets) or Size int, Gap time.Duration between pieces}
//		DropAfterBytes  map[string]int           // command → close the connection after this many bytes of its reply (0 = before any)
//		Abort           map[string]bool          // command → a close by one of its faults is abrupt: TCP RST, on TLS no close_notify
//		OverrideFinal   map[string][]string      // POST/IHAVE/TAKETHIS → lines written instead of the final reply (240/441, 235/437, 239/439); empty: none
//		DelayFinal      map[string]time.Duration // POST/IHAVE/TAKETHIS → sleep before the final reply
//		DropInUpload    map[string]int           // POST/IHAVE/TAKETHIS → close the connection after this many lines of the incoming dot block
//		Trailing        map[string]Trailing      // command → Trailing{Lines []string, Gap time.Duration}: more lines in a separate write after the reply and Gap
//	}
//	// Keys are upper-cased command names ("ARTICLE"), OverrideLine's the whole client line
//	// normalised (command upper-cased, arguments joined by single spaces); the hooks apply in
//	// Store mode, to a LineHandler and in Script mode (there: only to a matched step's reply), in this order:
//	// Delay, OverrideLine else Override, LongLine, DropInMultiline, then the write (Drip),
//	// then Endless (LineDelay pauses between the lines of the write). DropInMultiline counts the status line; 0 closes before writing anything.
//	// On a compressed connection the faults work on the inflated stream; each write is deflated, sync-flushed and sent in one socket write.
//	// CloseAfter counts every client line, mismatches included. Drip and Endless stop at the
//	// Deadline and close the connection.
//	// Byte level: Split and DropAfterBytes work on the bytes of the whole reply (offsets count from
//	// its first byte, status line included; Split.At wins over Size; Drip is the one-byte case and
//	// wins over Split); a reply of DropAfterBytes bytes or fewer is written whole. Trailing follows
//	// a reply that was written whole (not a cut or Endless one), also the 382 of STARTTLS. Abort is
//	// keyed by the command that was running when DropAfterBytes, DropInMultiline, DropInUpload or
//	// CloseAfter closed the connection; without it the close is normal (a normal close with unread
//	// client data pending still makes the OS send an RST). On plain TCP Abort sets SO_LINGER 0
//	// (the client reads ECONNRESET); on TLS it closes the TCP connection under the TLS layer, so
//	// the client sees a FIN after the last record and no close_notify (Go reads io.EOF at a
//	// record boundary, other stacks an error such as Python's SSLEOFError).
//	// The final reply of POST (and IHAVE in PeerHandler) takes only OverrideFinal, DelayFinal,
//	// Split, DropAfterBytes, Trailing and Abort (same command keys as the first reply: Split["POST"]
//	// cuts the 340 and the 240; offsets and byte counts are per reply, so use OverrideFinal and
//	// DelayFinal to target only the final one; Trailing also follows the 340/335 and the 435/436/480
//	// of IHAVE); the article is read and handled before. TAKETHIS has one reply that
//	// takes all faults, OverrideFinal over Override (and also on its 480 and 500 answers). DropInUpload counts the lines of the dot block
//	// read after the command (0 = none), the terminator not included; the cut block is not recorded.
//	const DefaultGreeting = "200 fakeserver ready"            // DefaultGreetingNoPost ("201 ...") for a read-only Store server
//
// Reply corpus (replycorpus.go): named wrong and malformed server replies for the test suite of
// an NNTP client. A case says which command it breaks, the lines to write instead and what is
// wrong; Apply puts it into the Options of a fake server (a command case needs a mode that
// applies Faults, e.g. a Store):
//
//	func ReplyCorpus() []ReplyCase                          // 112 cases
//	func ReplyCaseByName(name string) (ReplyCase, bool)
//	type ReplyCase struct {
//		Name         string   // unique, fit for t.Run, e.g. "group-211-nofields"
//		Family       Family   // FamilyCode, FamilyStatus, FamilyGreeting, FamilyBlock
//		Cmd          string   // the Faults key (upper-cased command) whose reply is replaced; "" for a greeting case
//		Line         string   // a sample client line ("GROUP g"); "" for a greeting case
//		Lines        []string // written instead of the normal reply, each + CRLF (a block carries its own "." line); may hold NUL, 8-bit, a bare LF; for a greeting case the greeting
//		Why          string   // what is wrong, with the RFC where there is one
//		Want         Outcome  // WantError (fail cleanly), WantEither (strict fails, lenient may accept; no panic, hang or desync), WantAccept (odd but legal: must be accepted)
//		NeedsTimeout bool     // the client waits for more data than the reply holds: the test needs a read timeout
//	}
//	func (rc ReplyCase) Apply(o *Options)                   // Faults.Override[Cmd]; Options.Greeting for a greeting case; adds to the maps already in o
//	func (rc ReplyCase) ApplyLine(o *Options)               // Apply for Line only (Faults.OverrideLine[Line])
//	func (rc ReplyCase) Bytes() []byte                      // exactly what the server writes: every line + CRLF
//	func (rc ReplyCase) IsGreeting() bool
//
// StoreHandler splits a command line on any Unicode white space (strings.Fields: also VT, FF and NBSP, not only SP and TAB; lenient, the RFC allows it) and ignores the keyword's case; an ARTICLE/HEAD/BODY/STAT/OVER/HDR argument that is neither a number (range) nor a message-id (<...>) is 501, also without a selected group.
//
// STARTTLS (RFC 4642): StoreHandler and PeerHandler offer it with StartTLS. It is listed in
// CAPABILITIES while the connection is plain, not compressed and not authenticated by AUTHINFO;
// the answer is 382 and the handshake follows its CRLF (a Handler calls Conn.StartTLS after
// writing the 382). Afterwards it is no longer listed, and a second STARTTLS gets 502, as on an
// implicit TLS connection (Options.TLS), after COMPRESS (TLS must come first; COMPRESS after
// TLS works) and after a successful AUTHINFO. The upgrade resets the session (Store mode: the
// selected group and article), and plaintext that the client pipelined behind STARTTLS closes
// the connection. Knobs: Refuse (reply instead of 382, e.g. a stripping attacker's 502 or 580;
// in Store mode Faults.Override "STARTTLS" works too, a "382" override upgrades),
// HideCapability, AuthNeedsTLS (RFC 4643: AUTHINFO 483 before TLS) and CloseAfterHandshake.
// The Server counts handshakes (implicit and STARTTLS) and records the SNI of every ClientHello.
//
// COMPRESS DEFLATE (RFC 8054): a Handler switches a connection with Conn.StartCompress right
// after it wrote the 206; StoreHandler and PeerHandler do it with Compress, a Script step with
// Step.Compress. Compression starts after the CRLF of the 206 in both directions: every write
// (replies, Faults, Raw().Write) is raw DEFLATE, sync-flushed at its end and sent in one socket
// write; the client stream is inflated as it arrives, whatever flush the client uses, so
// ReadLine, ReadDotBlock, Lines, Counts, Articles and Mismatches see the commands as the
// client wrote them. Corrupt input closes the connection (RFC 8054 2.2.2). The handlers answer
// COMPRESS while active with 502, any syntactically valid algorithm (RFC 8054 2.2.2), do not
// list COMPRESS DEFLATE in CAPABILITIES then, and answer AUTHINFO with 502.
// StartCompressOutputOnly (CompressOutputOnly in the options) models a half implementation for
// client tests: the 206 and everything after it is compressed, but the client's compressed
// commands are read as they are, so a client that follows the RFC gets no answer or "500"
// answers to binary lines.
//
// CompressShape (StoreOptions, PeerOptions, Step, Conn.StartCompressShaped) shapes the output
// stream for client tests; the zero value is the behaviour above:
//
//	type CompressShape struct {
//		Flush             CompressFlush // FlushSync (default); FlushPartial: zlib's Z_PARTIAL_FLUSH like INN's nnrpd (an empty fixed-Huffman block, no 00 00 FF FF marker, the last 0-7 bits unsent); FlushFinish: the first write after the 206 ends the stream, later writes are plain
//		Split             Split         // cut the compressed bytes of each write into socket writes: At (negative = from the end, -1..-3 inside the sync marker), Size, Gap (bounded by Deadline)
//		EmptyFlushWith206 bool          // the 206 line and an empty flush block in one socket write (Store, Peer, Script; not a Handler's own 206)
//		With206           []byte        // deflated and flushed into the 206's socket write (compressed payload pipelined behind the 206)
//		AfterEnd          []byte        // with FlushFinish: bytes after the final block, in its socket write
//	}
//
// FlushPartial writes literals in fixed-Huffman blocks (no matches) with a small bit writer,
// since compress/flate cannot do a partial flush; compress/flate's reader cannot read such a
// stream before the end either (it holds data until a stored block), the kit's inflater can.
package fakeserver

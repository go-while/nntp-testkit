package fakeserver

import (
	"strings"
	"sync"
	"time"
)

// PeerOptions configures PeerHandler, a fake transit peer: streaming (RFC 4644) and IHAVE
// (RFC 3977 6.3.2).
type PeerOptions struct {
	User, Pass      string          // non-empty: AUTHINFO required; CHECK/TAKETHIS/IHAVE before auth → 480
	Have            map[string]bool // ids the peer holds; guarded by the peer's mutex; TAKETHIS 239 and IHAVE 235 add to it
	Defer431Once    map[string]bool // CHECK of these ids → 431 the first time only
	Defer436Once    map[string]bool // IHAVE of these ids → 436 the first time only (initial reply: no article is sent)
	StallModeStream time.Duration   // sleep before the first MODE STREAM reply of the handler; capped at Options.Deadline
	OnArticle       func(msgID string, wire []string)
	// OnArticleRaw runs right after OnArticle with the article as the client wrote it: the
	// exact bytes of the dot block, still dot-stuffed, without the "." terminator (see
	// Conn.ReadDotBlockRaw); article.ParseDotBlock takes it. Same moment and goroutine.
	OnArticleRaw func(msgID string, raw []byte)

	// Compress offers COMPRESS DEFLATE (RFC 8054) as StoreOptions.Compress does:
	// "COMPRESS DEFLATE" in CAPABILITIES while compression is off, COMPRESS 206 (then both
	// directions compressed), 501, 502 once active, 503 for another algorithm, 480 before
	// required authentication; AUTHINFO answers 502 once compression is active. Off: COMPRESS
	// is unknown (500). innd itself has no COMPRESS (INN's nnrpd has).
	Compress bool
	// CompressOutputOnly offers COMPRESS like Compress, but after the 206 deflates only the
	// peer's output and reads the client's input uncompressed (Conn.StartCompressOutputOnly):
	// a broken peer, for client tests.
	CompressOutputOnly bool
	// CompressShape shapes the compressed output stream after the 206 (flush kind, split
	// writes, an empty flush with the 206, a final block); the zero value is the default.
	CompressShape CompressShape

	// Reject are the articles the peer refuses once it has read them, as a server whose
	// filter refuses the content does: CHECK still answers 238 (the id alone is wanted),
	// TAKETHIS answers 439 and IHAVE 335 then 437 after the article. A rejected article is
	// not added to Have and OnArticle does not run, so the next offer is rejected again. Ids
	// refused before any transfer are Have (CHECK 438, IHAVE 435, TAKETHIS 439).
	Reject map[string]bool

	// NoStream makes a peer without streaming, as innd with streaming turned off: MODE STREAM
	// answers "501 Unknown MODE variant", CHECK and TAKETHIS are unknown commands (500; the
	// article lines that a client sends after TAKETHIS are then read as commands, as innd
	// does) and CAPABILITIES does not list STREAMING. IHAVE works as before: innfeed and
	// innxmit fall back to it.
	NoStream bool

	// HoldGreeting holds the greeting until the channel is closed (or delivers a value): the
	// connection is accepted and counts in Open, MaxOpen and Accepted, the client waits for
	// its greeting. The hold ends at Options.Deadline (testkit.Deadline(), 60 s by default)
	// at the latest, and then the connection is dropped without a greeting: a test that
	// releases the channel later than that sees a closed connection, not a late greeting.
	// Server.Close ends the hold the same way. Needs Options.NoGreeting: the handler writes the
	// greeting (Options.Greeting) itself; NewPeer and StartPeer set it. Typical use: the test
	// dials K+1 times and asserts MaxOpen <= K, then closes the channel. A client that
	// disconnects while held is noticed when the hold ends.
	HoldGreeting <-chan struct{}
	// GreetingDelay waits this long before the greeting (after HoldGreeting), at most
	// Options.Deadline. Needs Options.NoGreeting like HoldGreeting (NewPeer sets it).
	GreetingDelay time.Duration
	// BeforeReply runs on the connection's goroutine before every reply is written (the 335
	// and the final 235/437 of IHAVE are two replies): cmd is the upper-cased command, line
	// the client line, status the first line of the reply about to be written (before Faults).
	// It may block, e.g. on Conn.Wait(ch), which returns when the test releases ch, at the
	// Deadline or when the server closes; a hook that blocks on anything else must watch
	// Server.Close itself. Counts, Have and OnArticle are not affected: they were done before
	// the hook runs.
	BeforeReply func(c *Conn, cmd, line, status string)
	// CapabilitiesReply, when non-nil, replaces the whole answer to CAPABILITIES (the lines
	// as written, a multi-line reply with its "." line), e.g. {"500 What?"} as Diablo's
	// feeder answers.
	CapabilitiesReply []string
	// ModeStreamReply, when non-nil, replaces the answer to MODE STREAM (also under NoStream),
	// e.g. {"203 StreamOK."} or {"501 no streaming"}. CHECK and TAKETHIS stay as they are
	// (NoStream makes them unknown).
	ModeStreamReply []string
	// DropAfterChecks > 0: on each connection the first DropAfterChecks CHECKs (after
	// authentication) are answered as usual, the next one gets DropReply (default
	// "400 Service temporarily unavailable") and the connection is closed.
	DropAfterChecks int
	DropReply       string
	// SilentAfterModeStream: after answering MODE STREAM the peer reads the client's lines
	// (recorded and counted as usual) and never answers again, until the client closes, the
	// server closes or Options.Deadline passes (then it closes the connection).
	SilentAfterModeStream bool
	// StartTLS offers STARTTLS (RFC 4642) as StoreOptions.StartTLS does: "STARTTLS" in
	// CAPABILITIES while the connection is plain, 382 and the upgrade, then 502; see
	// StartTLSOptions for the knobs. nil: STARTTLS is unknown (500).
	StartTLS *StartTLSOptions
}

// PeerHandler returns a Handler that answers like a transit peer: CAPABILITIES (VERSION 2,
// IHAVE, STREAMING unless NoStream (no READER: MODE READER answers 502), COMPRESS DEFLATE with Compress, STARTTLS with StartTLS), MODE
// STREAM/READER, AUTHINFO USER/PASS, CHECK, TAKETHIS, IHAVE, COMPRESS (with Compress or
// CompressOutputOnly), QUIT; anything else gets 500. Have is guarded by a mutex inside the
// handler: read it from a test only after the server is closed. OnArticle runs before the 239
// or 235 reply is written, on the connection's goroutine. The handler keeps its state (Have,
// the 431 and 436 deferrals, the stall) across the connections of one server.
//
// IHAVE <id> follows innd: 480 before authentication (no article is read, no 335 was sent);
// 435 for an id that is not "<...>" without white space (innd's answer, where RFC 3977 would
// allow 501); 435 when the peer has it; 436 when Defer436Once defers it (innd defers before
// the transfer when it is paused or the article is arriving on another connection); else 335,
// the article is read as a dot block (recorded in Articles), then 437 when Reject holds it or
// another connection delivered it meanwhile, else it is added to Have, OnArticle runs and the
// reply is 235.
func PeerHandler(o *PeerOptions) func(*Conn) {
	var mu sync.Mutex
	if o.Have == nil {
		o.Have = make(map[string]bool)
	}
	stallPending := o.StallModeStream > 0
	deferred := make(map[string]bool)      // ids already answered 431 once
	deferredIHAVE := make(map[string]bool) // ids already answered 436 once
	needAuth := o.User != "" || o.Pass != ""
	offerCompress := o.Compress || o.CompressOutputOnly
	const unknown = "500 fakeserver: unknown command"
	// take marks id as held unless it is rejected or already held; reject reports why not.
	take := func(id string) (reject bool) {
		mu.Lock()
		defer mu.Unlock()
		if o.Reject[id] || o.Have[id] {
			return true
		}
		o.Have[id] = true
		return false
	}
	dropReply := o.DropReply
	if dropReply == "" {
		dropReply = "400 Service temporarily unavailable"
	}
	return func(c *Conn) {
		c.shape, c.shapeOn = o.CompressShape, offerCompress
		if !o.greet(c) {
			return
		}
		authed := !needAuth
		checks := 0       // CHECKs answered on this connection (DropAfterChecks)
		userOK := false   // AUTHINFO USER named the configured user
		userSent := false // an AUTHINFO USER was seen (RFC 4643 §2.3.2: PASS before it is 482)
		for {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			verb, arg := line, ""
			if i := strings.IndexByte(line, ' '); i >= 0 {
				verb, arg = line[:i], line[i+1:]
			}
			verb = strings.ToUpper(verb)
			if o.NoStream && (verb == "CHECK" || verb == "TAKETHIS") {
				verb = "" // unknown: no article is read
			}
			var reply []string
			drop, silent, final := false, false, false
			switch verb {
			case "CAPABILITIES":
				reply = []string{"101 capability list follows", "VERSION 2", "IHAVE"}
				if !o.NoStream {
					reply = append(reply, "STREAMING")
				}
				if offerCompress && authed && !c.Compressed() {
					reply = append(reply, "COMPRESS DEFLATE")
				}
				if o.StartTLS.advertised(c, needAuth && authed) {
					reply = append(reply, "STARTTLS")
				}
				reply = append(reply, ".")
				if o.CapabilitiesReply != nil {
					reply = o.CapabilitiesReply
				}
			case "MODE":
				switch strings.ToUpper(arg) {
				case "STREAM":
					mu.Lock()
					stall := stallPending
					stallPending = false
					mu.Unlock()
					if stall && !c.sleep(o.StallModeStream) {
						return
					}
					reply = []string{"203 streaming ok"}
					if o.NoStream {
						reply = []string{"501 Unknown MODE variant"}
					}
					if o.ModeStreamReply != nil {
						reply = o.ModeStreamReply
					}
					silent = o.SilentAfterModeStream
				case "READER":
					reply = []string{"502 Transfer permission denied"} // innd with reader access denied: no READER, no MODE-READER capability
				default:
					reply = []string{unknown}
				}
			case "STARTTLS":
				if !o.StartTLS.on() {
					reply = []string{unknown}
				} else if arg != "" {
					reply = []string{"501 STARTTLS takes no arguments"}
				} else {
					reply = []string{c.startTLSLine(o.StartTLS, needAuth && authed)}
				}
			case "COMPRESS":
				switch {
				case !offerCompress:
					reply = []string{unknown}
				case !authed:
					reply = []string{"480 authentication required"}
				default:
					reply = []string{compressReply(strings.Fields(arg), c.Compressed())}
				}
			case "AUTHINFO":
				if o.StartTLS.needsTLS(c, needAuth) {
					reply = []string{"483 Encryption required"}
					break
				}
				if c.Compressed() {
					reply = []string{"502 compression active"}
					break
				}
				if authed && needAuth {
					reply = []string{"502 Command unavailable"} // RFC 4643 §2.3: no AUTHINFO after a successful one
					break
				}
				sub, val := arg, ""
				if i := strings.IndexByte(arg, ' '); i >= 0 {
					sub, val = arg[:i], arg[i+1:]
				}
				switch strings.ToUpper(sub) {
				case "USER":
					userSent = true
					userOK = o.User == "" || val == o.User
					if userOK {
						reply = []string{"381 password required"}
					} else {
						reply = []string{"481 authentication failed"}
					}
				case "PASS":
					switch {
					case !userSent:
						reply = []string{"482 Authentication commands issued out of sequence"}
					case userOK && (o.Pass == "" || val == o.Pass):
						authed = true
						reply = []string{"281 authentication accepted"}
					default:
						userOK, userSent = false, false // start again with AUTHINFO USER
						reply = []string{"481 authentication failed"}
					}
				default:
					reply = []string{unknown}
				}
			case "CHECK":
				if !authed {
					reply = []string{"480 authentication required"}
					break
				}
				checks++
				if o.DropAfterChecks > 0 && checks > o.DropAfterChecks {
					reply, drop = []string{dropReply}, true
					break
				}
				mu.Lock()
				switch {
				case o.Have[arg]:
					reply = []string{"438 " + arg}
				case o.Defer431Once[arg] && !deferred[arg]:
					deferred[arg] = true
					reply = []string{"431 " + arg}
				default:
					reply = []string{"238 " + arg}
				}
				mu.Unlock()
			case "TAKETHIS":
				wire, raw, err := c.ReadDotBlockRaw()
				if err != nil {
					return
				}
				if !authed {
					reply = []string{"480 authentication required"}
					break
				}
				if take(arg) {
					reply = []string{"439 " + arg}
					break
				}
				if o.OnArticle != nil {
					o.OnArticle(arg, wire)
				}
				if o.OnArticleRaw != nil {
					o.OnArticleRaw(arg, raw)
				}
				reply = []string{"239 " + arg}
			case "IHAVE":
				if !authed {
					reply = []string{"480 authentication required"}
					break
				}
				if !validMessageID(arg) {
					reply = []string{"435 Syntax error in message-ID"}
					break
				}
				mu.Lock()
				switch {
				case o.Have[arg]:
					reply = []string{"435 Duplicate"}
				case o.Defer436Once[arg] && !deferredIHAVE[arg]:
					deferredIHAVE[arg] = true
					reply = []string{"436 Retry later"}
				}
				mu.Unlock()
				if reply != nil {
					break
				}
				if !o.send(c, line, []string{"335 Send it"}, false) {
					return
				}
				if !strings.HasPrefix(c.last, "335") {
					// Faults.Override made the first reply final: no article follows
					if c.closeDue() {
						return
					}
					continue
				}
				wire, raw, err := c.ReadDotBlockRaw()
				if err != nil {
					return
				}
				if take(arg) {
					reply = []string{"437 Rejected " + arg}
					final = true
					break
				}
				if o.OnArticle != nil {
					o.OnArticle(arg, wire)
				}
				if o.OnArticleRaw != nil {
					o.OnArticleRaw(arg, raw)
				}
				reply = []string{"235 Article transferred OK"}
				final = true // the 235 is the final reply: OverrideFinal etc., as POST's 240 in StoreHandler
			case "QUIT":
				_ = o.send(c, line, []string{"205 bye"}, false)
				return
			default:
				reply = []string{unknown}
			}
			if o.StartTLS.needsTLS(c, needAuth) && len(reply) > 0 && strings.HasPrefix(reply[0], "480") {
				reply[0] = "483 Encryption required" // authentication is not possible before TLS
			}
			if !o.send(c, line, reply, final) {
				return
			}
			if drop {
				return
			}
			if silent {
				o.goSilent(c)
				return
			}
			if offerCompress && !c.compressAfter(verb, c.last, o.CompressOutputOnly) {
				return
			}
			// the upgrade before the CloseAfter check, as in the Store handler: a STARTTLS that
			// is the last command before the close still gets its handshake
			if upgraded, ok := c.startTLSAfter(o.StartTLS, verb, c.last); !ok {
				return
			} else if upgraded {
				userOK, userSent = false, false // RFC 4642 2.2.2.1
			}
			if c.closeDue() {
				return
			}
		}
	}
}

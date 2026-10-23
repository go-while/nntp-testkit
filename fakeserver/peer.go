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
}

// PeerHandler returns a Handler that answers like a transit peer: CAPABILITIES (VERSION 2,
// IHAVE, READER, STREAMING unless NoStream), MODE STREAM/READER, AUTHINFO USER/PASS, CHECK,
// TAKETHIS, IHAVE, QUIT; anything else gets 500. Have is guarded by a mutex inside the
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
	return func(c *Conn) {
		authed := !needAuth
		userOK := false
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
			switch verb {
			case "CAPABILITIES":
				reply = []string{"101 capability list follows", "VERSION 2", "IHAVE", "READER"}
				if !o.NoStream {
					reply = append(reply, "STREAMING")
				}
				reply = append(reply, ".")
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
				case "READER":
					reply = []string{"200 reader"}
				default:
					reply = []string{unknown}
				}
			case "AUTHINFO":
				sub, val := arg, ""
				if i := strings.IndexByte(arg, ' '); i >= 0 {
					sub, val = arg[:i], arg[i+1:]
				}
				switch strings.ToUpper(sub) {
				case "USER":
					userOK = o.User == "" || val == o.User
					if userOK {
						reply = []string{"381 password required"}
					} else {
						reply = []string{"481 authentication failed"}
					}
				case "PASS":
					if userOK && (o.Pass == "" || val == o.Pass) {
						authed = true
						reply = []string{"281 authentication accepted"}
					} else {
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
				wire, err := c.ReadDotBlock()
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
				if err := c.WriteLines("335 Send it"); err != nil {
					return
				}
				wire, err := c.ReadDotBlock()
				if err != nil {
					return
				}
				if take(arg) {
					reply = []string{"437 Rejected " + arg}
					break
				}
				if o.OnArticle != nil {
					o.OnArticle(arg, wire)
				}
				reply = []string{"235 Article transferred OK"}
			case "QUIT":
				_ = c.WriteLines("205 bye")
				return
			default:
				reply = []string{unknown}
			}
			if err := c.WriteLines(reply...); err != nil {
				return
			}
		}
	}
}

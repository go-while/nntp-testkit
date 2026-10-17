package fakeserver

import (
	"strings"
	"sync"
	"time"
)

// PeerOptions configures PeerHandler, a fake streaming (RFC 4644) peer.
type PeerOptions struct {
	User, Pass      string          // non-empty: AUTHINFO required; CHECK/TAKETHIS before auth → 480
	Have            map[string]bool // ids the peer holds; guarded by the peer's mutex; TAKETHIS 239 adds to it
	Defer431Once    map[string]bool // CHECK of these ids → 431 the first time only
	StallModeStream time.Duration   // sleep before the first connection's "203" reply; capped at Options.Deadline
	OnArticle       func(msgID string, wire []string)
}

// PeerHandler returns a Handler that answers like a streaming peer: CAPABILITIES, MODE
// STREAM/READER, AUTHINFO USER/PASS, CHECK, TAKETHIS, QUIT; anything else gets 500. Have is
// guarded by a mutex inside the handler: read it from a test only after the server is closed.
// OnArticle runs before the 239 reply is written, on the connection's goroutine. The handler
// keeps its state (Have, the 431 deferrals, the stall) across the connections of one server.
func PeerHandler(o *PeerOptions) func(*Conn) {
	var mu sync.Mutex
	if o.Have == nil {
		o.Have = make(map[string]bool)
	}
	stallPending := o.StallModeStream > 0
	deferred := make(map[string]bool) // ids already answered 431 once
	needAuth := o.User != "" || o.Pass != ""
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
			var reply []string
			switch strings.ToUpper(verb) {
			case "CAPABILITIES":
				reply = []string{"101 capability list follows", "VERSION 2", "READER", "STREAMING", "."}
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
				case "READER":
					reply = []string{"200 reader"}
				default:
					reply = []string{"500 fakeserver: unknown command"}
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
					reply = []string{"500 fakeserver: unknown command"}
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
				mu.Lock()
				held := o.Have[arg]
				o.Have[arg] = true
				mu.Unlock()
				if held {
					reply = []string{"439 " + arg}
					break
				}
				if o.OnArticle != nil {
					o.OnArticle(arg, wire)
				}
				reply = []string{"239 " + arg}
			case "QUIT":
				_ = c.WriteLines("205 bye")
				return
			default:
				reply = []string{"500 fakeserver: unknown command"}
			}
			if err := c.WriteLines(reply...); err != nil {
				return
			}
		}
	}
}

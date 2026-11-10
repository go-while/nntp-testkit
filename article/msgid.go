package article

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// DefaultDomain is the domain of the Message-IDs MsgID makes; .invalid never resolves
// (RFC 2606), so no test article can be mistaken for a real one.
const DefaultDomain = "testkit.invalid"

// maxNameLen caps the part of a Message-ID that comes from the test name.
const maxNameLen = 100

// msgSeq numbers the Message-IDs of the process.
var msgSeq atomic.Uint64

// MsgID returns a new Message-ID, unique in the process and across runs, made from the test
// name: "<Name.time.seq@testkit.invalid>" (see NewMsgID).
func MsgID(tb testing.TB) string {
	tb.Helper()
	return NewMsgID(tb.Name(), DefaultDomain)
}

// MsgIDAt is MsgID with the domain you choose after the "@". The domain is used as given.
func MsgIDAt(tb testing.TB, domain string) string {
	tb.Helper()
	return NewMsgID(tb.Name(), domain)
}

// NewMsgID returns "<name.time.seq@domain>": name with every character that is not a letter,
// digit, '-' or '_' replaced by '_' (a Go test name such as "TestX/sub case#01" holds spaces,
// slashes and '#'; it is cut to 100 bytes, "t" when empty), the time in nanoseconds in base 36,
// and a sequence number that counts up in the process, so two calls never return the same
// string, also from parallel goroutines, and two runs against a server that keeps its history
// differ. domain "" is DefaultDomain; a domain is not checked. With a valid domain
// (letters, digits, '-' and dots) the result satisfies ValidMsgID and is far below 250 bytes.
func NewMsgID(name, domain string) string {
	if domain == "" {
		domain = DefaultDomain
	}
	return "<" + sanitize(name) + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + "." +
		strconv.FormatUint(msgSeq.Add(1), 10) + "@" + domain + ">"
}

// sanitize turns a test name into an id-left part made of letters, digits, '-' and '_'.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		if b.Len() >= maxNameLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "t"
	}
	return b.String()
}

// ValidMsgID reports whether id is a Message-ID in the form generators write (RFC 5536
// section 3.1.3, RFC 3977 section 3.6): at most 250 bytes, "<" id-left "@" id-right ">", both
// sides dot-atom-text (atext characters, single dots only between them). The quoted id-left
// and the literal id-right that RFC 5536 also allows are not accepted.
func ValidMsgID(id string) bool {
	if len(id) < 5 || len(id) > 250 || id[0] != '<' || id[len(id)-1] != '>' {
		return false
	}
	left, right, ok := strings.Cut(id[1:len(id)-1], "@")
	return ok && dotAtom(left) && dotAtom(right)
}

func dotAtom(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			if !atext(part[i]) {
				return false
			}
		}
	}
	return true
}

// atext is RFC 5322's atext.
func atext(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}

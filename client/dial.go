package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// dialPoll is the pause between the attempts of TryDialUntil.
const dialPoll = 50 * time.Millisecond

// TryDialFrom is TryDial from a chosen local source address: localIP ("127.0.0.2", "::1", no
// port) is bound before the connect, for servers that sort peers by source IP. The kernel must
// own the address (Linux: all of 127.0.0.0/8; macOS needs an alias), else the dial fails. The
// greeting is read, deadline is the hang guard as in TryDial, the caller closes the Conn.
func TryDialFrom(addr, localIP string, deadline time.Duration) (*Conn, error) {
	ip := net.ParseIP(localIP)
	if ip == nil {
		return nil, fmt.Errorf("local address %q is not an IP address", localIP)
	}
	return tryDialer(addr, deadline, &net.TCPAddr{IP: ip})
}

// DialFrom is TryDialFrom with the default hang guard, tb.Cleanup(Close),
// a failure fails tb.
func DialFrom(tb testing.TB, addr, localIP string) *Conn {
	tb.Helper()
	c, err := TryDialFrom(addr, localIP, 0)
	if err != nil {
		tb.Fatalf("dial %s from %s: %v", addr, localIP, err)
	}
	return c.WithTB(tb)
}

// tryDialer dials addr (from local when set), reads the greeting and returns a strict Conn.
func tryDialer(addr string, deadline time.Duration, local net.Addr) (*Conn, error) {
	c := &Conn{Deadline: deadline, Strict: true}
	d := net.Dialer{Timeout: c.deadline(), LocalAddr: local}
	nc, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c.Conn = nc
	c.R = bufio.NewReaderSize(nc, readerSize)
	if _, err := c.ReadGreeting(); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

// TryDialUntil dials addr again and again, every 50 ms, until the greeting starts with prefix
// ("200", "201"), for a server whose first greetings are 400 while it starts. A refused dial,
// a missing greeting and a greeting with another start count as not yet. d bounds the whole
// wait (0 = testkit.Deadline()); on success the Conn has the default hang guard. A timeout
// error names the last greeting or error seen. The caller closes the Conn; the connections
// that did not match are closed here.
func TryDialUntil(addr, prefix string, d time.Duration) (*Conn, error) {
	if d <= 0 {
		d = testkit.Deadline()
	}
	limit := time.Now().Add(d)
	last := "no attempt finished"
	for {
		left := time.Until(limit)
		if left <= 0 {
			return nil, fmt.Errorf("no greeting starting with %q from %s within %v (last: %s)", prefix, addr, d, last)
		}
		c, err := tryDialer(addr, left, nil)
		if err == nil {
			if strings.HasPrefix(c.Greeting, prefix) {
				c.Deadline = 0
				return c, nil
			}
			last = fmt.Sprintf("greeting %q", c.Greeting)
			c.Close()
		} else {
			last = err.Error()
		}
		time.Sleep(min(dialPoll, max(time.Until(limit), 0)))
	}
}

// DialUntil is TryDialUntil that registers tb.Cleanup(Close) and fails tb on a timeout.
func DialUntil(tb testing.TB, addr, prefix string, d time.Duration) *Conn {
	tb.Helper()
	c, err := TryDialUntil(addr, prefix, d)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return c.WithTB(tb)
}

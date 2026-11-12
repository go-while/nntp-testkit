package fakeserver

import "time"

// Test knobs of PeerHandler that hold, slow or cut a connection or change a reply. They live
// here so that peer.go keeps the protocol logic only; the fields are declared in PeerOptions.
//
// Every wait is bounded by Options.Deadline and ends when the server is closed, so a held
// connection never outlives Server.Close (and no goroutine is started for it: the wait runs on
// the connection's own goroutine, which Close already waits for).

// Wait blocks the connection's goroutine until ch is closed or delivers a value (true), the
// server's Deadline passes or the server is closed (false). A nil ch waits for the Deadline or
// Close only. It is for a Handler or a PeerOptions.BeforeReply that must hold a reply until
// the test says so:
//
//	release := make(chan struct{})
//	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(&fakeserver.PeerOptions{
//		BeforeReply: func(c *fakeserver.Conn, cmd, line, status string) {
//			if cmd == "CHECK" {
//				c.Wait(release)
//			}
//		},
//	})})
func (c *Conn) Wait(ch <-chan struct{}) bool {
	tm := time.NewTimer(c.s.deadline)
	defer tm.Stop()
	select {
	case <-ch:
		return true
	case <-tm.C:
		return false
	case <-c.s.done:
		return false
	}
}

// Sleep waits d, at most the server's Deadline, or until the server is closed; false when it
// was closed meanwhile (the connection is going away: stop serving it).
func (c *Conn) Sleep(d time.Duration) bool { return c.sleep(d) }

// greet writes the peer's greeting itself when GreetingDelay or HoldGreeting asks for it
// (Options.NoGreeting must be set then, else the server has greeted already and the knobs
// cannot work: the test fails). It first waits for HoldGreeting, then sleeps GreetingDelay,
// then writes Options.Greeting. False: the server closed or the Deadline passed while held, or
// the write failed; the connection is to be dropped. The connection counts as open (Open,
// MaxOpen) the whole time.
func (o *PeerOptions) greet(c *Conn) bool {
	if o.HoldGreeting == nil && o.GreetingDelay <= 0 {
		return true
	}
	if !c.s.o.NoGreeting {
		if c.s.tb != nil {
			c.s.tb.Errorf("fakeserver: PeerOptions.HoldGreeting and GreetingDelay need Options.NoGreeting (the server greeted already)")
		}
		return true
	}
	if o.HoldGreeting != nil && !c.Wait(o.HoldGreeting) {
		return false
	}
	if o.GreetingDelay > 0 && !c.sleep(o.GreetingDelay) {
		return false
	}
	return c.WriteLines(c.s.o.Greeting) == nil
}

// send writes a reply of the peer: BeforeReply first, then the reply through the Faults
// (Delay, Override, Drip, ...); final: the final reply of IHAVE, with the faults meant for it (Conn.writeFinal). False: stop serving the connection.
func (o *PeerOptions) send(c *Conn, line string, reply []string, final bool) bool {
	if o.BeforeReply != nil {
		status := ""
		if len(reply) > 0 {
			status = reply[0]
		}
		o.BeforeReply(c, command(line), line, status)
		select {
		case <-c.s.done: // closed while the reply was held: it is not sent
			return false
		default:
		}
	}
	switch {
	case final:
		return c.writeFinal(line, reply...)
	case command(line) == "TAKETHIS":
		return c.writeKind(line, replySole, reply)
	}
	return c.writeReply(line, reply...)
}

// goSilent reads and records the client's lines, answering none, until the client closes, the
// server closes or the Deadline passes (then the connection is closed).
func (o *PeerOptions) goSilent(c *Conn) {
	_ = c.conn.SetReadDeadline(time.Now().Add(c.s.deadline))
	for {
		if _, err := c.ReadLine(); err != nil {
			return
		}
	}
}

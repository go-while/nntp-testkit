package fakeserver

// LineHandler returns a Handler that reads the client's lines one at a time, in any order, and
// answers each with what f returns: the reply lines (CRLF is appended to each; a multi-line
// reply includes its "." line; none writes nothing) and whether to close the connection after
// them. It is for a peer that Script cannot express (the client's commands come in an order the
// test does not fix) and that needs none of the state of StoreHandler or PeerHandler.
//
// f gets the connection (Conn.ID tells the connections apart; f runs on the connection's
// goroutine, so guard what it shares between connections) and the line as sent, line end
// stripped. Every line is read with Conn.ReadLine, so it is in Lines and Counts, also the lines
// of an article that the client sends after POST, IHAVE or TAKETHIS; to take an article as one
// block, f calls c.ReadDotBlock before it returns the reply. f may also write itself
// (c.WriteLines, c.Raw()) and return no lines.
//
// Options.Faults apply to the replies as in Script mode, the line being the client line f got:
// Delay, Override, OverrideLine, Drip, CloseAfter and the rest.
func LineHandler(f func(c *Conn, line string) (reply []string, closeConn bool)) func(*Conn) {
	return func(c *Conn) {
		for {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			reply, done := f(c, line)
			if !c.writeReply(line, reply...) || done || c.closeDue() {
				return
			}
		}
	}
}

package fakeserver

import (
	"bufio"
	"errors"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/internal/inflate"
)

// StartCompress switches the connection to COMPRESS DEFLATE (RFC 8054) in both directions: call
// it right after writing the "206" reply, since the compression layer starts after its CRLF.
// From then on every write (WriteLines, the replies of the handlers with their Faults,
// Raw().Write) is deflated (raw DEFLATE, RFC 1951, no zlib header), sync-flushed at its end and
// sent to the socket in one write, and ReadLine and ReadDotBlock read the inflated client
// stream, so Lines, Counts, Articles and Mismatches record the commands as the client wrote
// them. Bytes the client sent after its COMPRESS line before the 206 (a pipelined COMPRESS,
// which RFC 8054 2.2.1 forbids) are inflated too: plain text there is corrupt input (INN's
// nnrpd reads such buffered bytes as plain commands instead). The input is inflated by a
// goroutine that passes on every byte as soon as it can be decoded, whatever flush the client
// uses (sync, full or zlib's partial flush). Corrupt compressed input is a read error: the
// handlers return and the connection is closed, as RFC 8054 2.2.2 requires. Raw returns the
// compressing conn afterwards; keep Raw() from before the call to write bytes past the
// compressor (corrupt output for a client test). It fails when compression is already on.
func (c *Conn) StartCompress() error { return c.startCompress(false, CompressShape{}) }

// StartCompressShaped is StartCompress with the output stream shaped (CompressShape): flush
// kind, splits, a final block. EmptyFlushWith206 and With206 have no effect here, since the 206
// is already written; the handlers of the package write them with the 206.
func (c *Conn) StartCompressShaped(shape CompressShape) error { return c.startCompress(false, shape) }

// StartCompressOutputOnly is a broken COMPRESS for client tests: after the 206 only what the
// server writes is deflated, while the client's bytes are read as they arrive, without
// inflating them, as a server that compresses its responses but never decompresses its
// input. A client that compresses its commands, as RFC 8054 requires, then gets no answer
// (its deflated bytes rarely hold a line end) or "500" answers to binary lines, and Lines
// records those raw bytes.
func (c *Conn) StartCompressOutputOnly() error { return c.startCompress(true, CompressShape{}) }

// Compressed reports whether StartCompress or StartCompressOutputOnly switched the connection
// to compression.
func (c *Conn) Compressed() bool {
	_, ok := c.conn.(*deflateConn)
	return ok
}

func (c *Conn) startCompress(outputOnly bool, shape CompressShape) error {
	if c.Compressed() {
		return errors.New("fakeserver: compression is already active")
	}
	enc := c.enc // the one that wrote the pipelined bytes of the 206, when it did
	c.enc = nil
	if enc == nil {
		var err error
		if enc, err = newEncoder(shape); err != nil {
			return err
		}
	}
	d := &deflateConn{Conn: c.conn, r: c.r, enc: enc, sleep: c.sleep, deadline: c.s.deadline}
	if !outputOnly {
		pr, pw := io.Pipe()
		d.r, d.pr = pr, pr
		go func(src *bufio.Reader) {
			_ = pw.CloseWithError(inflate.Run(src, func(b []byte) error {
				_, err := pw.Write(b)
				return err
			}))
		}(c.r)
	}
	c.conn = d
	c.r = newReader(d)
	return nil
}

// deflateConn is the conn of a compressed connection: writes are deflated and flushed, reads
// come from r (the inflater, or the plain reader for StartCompressOutputOnly). Deadlines,
// addresses and Close are those of the conn underneath.
type deflateConn struct {
	net.Conn
	r        io.Reader
	pr       *io.PipeReader // the inflater's output; nil for StartCompressOutputOnly
	enc      *encoder
	sleep    func(time.Duration) bool // Conn.sleep: bounded by the Deadline, false when the server closed
	deadline time.Duration
	werr     error     // the first failed write: the stream is broken from there on
	wdl      time.Time // the write deadline last set through this conn (zero: none)
}

// SetWriteDeadline and SetDeadline remember the write deadline, so that send can put back the
// one writeKind set (Drip, LineDelay, Split, Endless) instead of clearing it.
func (d *deflateConn) SetWriteDeadline(t time.Time) error {
	d.wdl = t
	return d.Conn.SetWriteDeadline(t)
}

func (d *deflateConn) SetDeadline(t time.Time) error {
	d.wdl = t
	return d.Conn.SetDeadline(t)
}

func (d *deflateConn) Read(p []byte) (int, error) { return d.r.Read(p) }

// Close closes the conn underneath and the inflater's pipe, which ends its goroutine.
func (d *deflateConn) Close() error {
	if d.pr != nil {
		_ = d.pr.Close()
	}
	return d.Conn.Close()
}

// Write deflates p, flushes the compressor as the shape says (sync flush by default) and
// writes the result to the conn underneath in one write (or the pieces of the shape's Split),
// as a server that flushes its output buffer, so the client can inflate all of p at once
// (compress/flate's Writer on its own writes a short input in five pieces). After a failed
// write the deflate stream is broken, so every later Write returns that error.
func (d *deflateConn) Write(p []byte) (int, error) {
	if d.werr != nil {
		return 0, d.werr
	}
	b, err := d.enc.write(p)
	if err != nil {
		return 0, err
	}
	if err := d.send(b); err != nil {
		d.werr = err
		return 0, err
	}
	return len(p), nil
}

// send writes b to the conn underneath, cut by the shape's Split. The pauses between the
// pieces end at the server's Deadline from the call, with a write deadline set meanwhile.
func (d *deflateConn) send(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	sp := d.enc.shape.Split
	cuts := sp.cuts(len(b))
	if len(cuts) == 0 {
		_, err := d.Conn.Write(b)
		return err
	}
	end := time.Now().Add(d.deadline)
	if sp.Gap > 0 && (d.wdl.IsZero() || end.Before(d.wdl)) {
		_ = d.Conn.SetWriteDeadline(end)
		defer d.Conn.SetWriteDeadline(d.wdl) // what the caller had set, not "none"
	}
	prev := 0
	for _, k := range append(cuts, len(b)) {
		if _, err := d.Conn.Write(b[prev:k]); err != nil {
			return err
		}
		prev = k
		if k < len(b) && sp.Gap > 0 && (time.Now().After(end) || !d.sleep(sp.Gap)) {
			return errors.New("fakeserver: compressed split ran past the Deadline or the server closed")
		}
	}
	return nil
}

// algorithm is the syntax of an RFC 8054 algorithm name (5.3): 1 to 20 upper-case letters,
// digits, "-" or "_"; case-sensitive.
var algorithm = regexp.MustCompile(`^[A-Z0-9_-]{1,20}$`)

// compressReply is the RFC 8054 2.2 answer of a server that offers COMPRESS DEFLATE to
// "COMPRESS args": 501 for a missing, extra or syntactically invalid algorithm, 502 when
// compression is already active, 503 for an algorithm other than DEFLATE, else 206.
func compressReply(args []string, active bool) string {
	switch {
	case len(args) != 1 || !algorithm.MatchString(args[0]):
		return "501 Syntax error in compression algorithm"
	case active:
		return "502 Compression already active"
	case args[0] != "DEFLATE":
		return "503 Compression algorithm not supported"
	}
	return "206 Compression active"
}

// compressAfter starts compression after the reply status to a COMPRESS command when that
// reply was a 206 and compression is not active yet. False when it could not be started.
func (c *Conn) compressAfter(cmd, status string, outputOnly bool) bool {
	if cmd != "COMPRESS" || (status != "206" && !strings.HasPrefix(status, "206 ")) || c.Compressed() {
		return true
	}
	return c.startCompress(outputOnly, c.shape) == nil
}

// pipeline206 returns the compressed bytes the shape writes in the same socket write as the
// 206 reply to a command (nil for none), and keeps the encoder for startCompress. writeKind
// calls it with the reply's status line; armed is set for the command that starts compression
// (COMPRESS, or any command of a Script step with Compress).
func (c *Conn) pipeline206(armed bool, status string) []byte {
	if !armed || !c.shape.pipelined() || c.Compressed() || (status != "206" && !strings.HasPrefix(status, "206 ")) {
		return nil
	}
	enc, err := newEncoder(c.shape)
	if err != nil {
		return nil
	}
	b, err := enc.flushData(c.shape.With206)
	if err != nil {
		return nil
	}
	c.enc = enc
	return slices.Clone(b)
}

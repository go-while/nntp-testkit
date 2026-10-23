package fakeserver

import (
	"bufio"
	"compress/flate"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/go-while/nntp-testkit/internal/inflate"
)

// StartCompress switches the connection to COMPRESS DEFLATE (RFC 8054) in both directions:
// call it right after writing the "206" reply, since the compression layer starts after its
// CRLF. From then on every write (WriteLines, the replies of the handlers with their Faults,
// Raw().Write) is deflated (raw DEFLATE, RFC 1951, no zlib header) and sync-flushed at its
// end, and ReadLine and ReadDotBlock read the inflated client stream, so Lines, Counts,
// Articles and Mismatches record the commands as the client wrote them. Bytes the client
// sent after its COMPRESS line before the 206 (a pipelined COMPRESS, which RFC 8054 2.2.1
// forbids) are inflated too: plain text there is corrupt input (INN's nnrpd reads such
// buffered bytes as plain commands instead). The input is inflated by a goroutine that
// passes on every byte as soon as it can be decoded, whatever flush the client uses (sync,
// full or zlib's partial flush). Corrupt compressed input is a read error: the handlers
// return and the connection is closed, as RFC 8054 2.2.2 requires. Raw returns the
// compressing conn afterwards; keep Raw() from before the call to write bytes past the
// compressor (corrupt output for a client test). It fails when compression is already on.
func (c *Conn) StartCompress() error { return c.startCompress(false) }

// StartCompressOutputOnly is a broken COMPRESS for client tests: after the 206 only what the
// server writes is deflated, while the client's bytes are read as they arrive, without
// inflating them, as a server that compresses its responses but never decompresses its
// input. A client that compresses its commands, as RFC 8054 requires, then gets no answer
// (its deflated bytes rarely hold a line end) or "500" answers to binary lines, and Lines
// records those raw bytes.
func (c *Conn) StartCompressOutputOnly() error { return c.startCompress(true) }

// Compressed reports whether StartCompress or StartCompressOutputOnly switched the connection
// to compression.
func (c *Conn) Compressed() bool {
	_, ok := c.conn.(*deflateConn)
	return ok
}

func (c *Conn) startCompress(outputOnly bool) error {
	if c.Compressed() {
		return errors.New("fakeserver: compression is already active")
	}
	zw, err := flate.NewWriter(c.conn, flate.DefaultCompression)
	if err != nil {
		return err
	}
	d := &deflateConn{Conn: c.conn, r: c.r, zw: zw}
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
	r  io.Reader
	pr *io.PipeReader // the inflater's output; nil for StartCompressOutputOnly
	zw *flate.Writer
}

func (d *deflateConn) Read(p []byte) (int, error) { return d.r.Read(p) }

// Close closes the conn underneath and the inflater's pipe, which ends its goroutine.
func (d *deflateConn) Close() error {
	if d.pr != nil {
		_ = d.pr.Close()
	}
	return d.Conn.Close()
}

// Write deflates p and flushes the compressor (sync flush), so the client can inflate all of
// p at once.
func (d *deflateConn) Write(p []byte) (int, error) {
	if _, err := d.zw.Write(p); err != nil {
		return 0, err
	}
	if err := d.zw.Flush(); err != nil {
		return 0, err
	}
	return len(p), nil
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
	return c.startCompress(outputOnly) == nil
}

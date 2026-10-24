package client

import (
	"bufio"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-while/nntp-testkit/internal/inflate"
)

// pendingMax bounds the inflated bytes held for the reader of a compressed Conn: the inflater
// waits while that much is unread, so a server that never stops writing cannot fill memory.
const pendingMax = 1 << 20

// TryCompress sends "COMPRESS DEFLATE" (RFC 8054) uncompressed and reads the reply. On 206 it
// switches the Conn to compression with StartCompress and returns the 206 line and nil; any
// other reply returns its code and line with an error, and the Conn stays uncompressed (403,
// 501, 502, 503 per RFC 8054 2.2.1; 500 when the server lacks the command). It does not keep
// a test from sending COMPRESS twice: on a compressed Conn the command goes out compressed
// and a correct server answers 502.
func (c *Conn) TryCompress() (int, string, error) {
	code, line, err := c.TryCmd("COMPRESS DEFLATE")
	if err != nil {
		return code, line, err
	}
	if code != 206 {
		return code, line, fmt.Errorf("COMPRESS DEFLATE: %w", ExpectCode(code, line, 206))
	}
	return code, line, c.StartCompress()
}

// Compress is TryCompress that fails tb unless the server answers 206 and compression is on.
func (c *Conn) Compress() {
	if c.tb != nil {
		c.tb.Helper()
	}
	if _, line, err := c.TryCompress(); err != nil {
		c.fatalf("compress: %v (line %q)", err, line)
	}
}

// StartCompress switches c to COMPRESS DEFLATE without sending anything: for a test that sent
// the COMPRESS command itself and read the 206. From then on c.Conn is a net.Conn that
// deflates every Write (raw DEFLATE, RFC 1951, no zlib header) with a sync flush at its end
// and sends the result to the socket in one write, so each Send is decodable by the server at
// once but not before the write of it returned (a server that answers a command and closes,
// as on QUIT, cannot make the rest of that command's write fail), and inflates what the
// server sends; c.R
// is a new reader on top of it. Bytes already buffered in the old c.R after the 206 line are
// the start of the compressed stream and are inflated first. Every inflated byte is readable
// as soon as the bytes that carry it have arrived, whatever flush the server uses: a sync or
// full flush (00 00 ff ff) or zlib's Z_PARTIAL_FLUSH (an empty fixed block), which INN's
// nnrpd uses and which compress/flate's reader would hold back until more data comes.
//
// The socket is read by an inflater goroutine without a read deadline (Close ends it); the
// read deadline that every blocking step sets applies to the inflated stream instead, so the
// hang guard, ExpectNoPending and direct deadlines on c.Conn work as before, and a timeout
// loses nothing. Writes keep the socket's write deadline. Keep the old c.Conn before the call
// to write bytes past the compressor (corrupt input for a server test).
//
// A connection the server closes reads as io.EOF, as without compression; output the server
// did not flush before closing is lost (the reply is missing or cut). A corrupt compressed
// stream is a read error wrapping flate.CorruptInputError, after the bytes inflated before
// it, and every later read returns it; the Conn stays open (RFC 8054 2.2.2 has the receiver
// close it: call Close). StartCompress fails when compression is already on. Call it on the
// Conn, not on a view from For.
func (c *Conn) StartCompress() error {
	if c.Compressed() {
		return errors.New("client: compression is already active")
	}
	if err := c.Conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	d := &deflateConn{raw: c.Conn, wake: make(chan struct{}, 1)}
	zw, err := flate.NewWriter(&d.out, flate.DefaultCompression)
	if err != nil {
		return err
	}
	d.zw = zw
	d.room = sync.NewCond(&d.mu)
	go d.inflate(c.R)
	c.Conn = d
	c.R = bufio.NewReaderSize(d, readerSize)
	c.bump()
	return nil
}

// Compressed reports whether StartCompress (or a successful Compress) switched c to
// compression.
func (c *Conn) Compressed() bool {
	_, ok := c.Conn.(*deflateConn)
	return ok
}

// deflateConn is c.Conn after StartCompress: writes are deflated and flushed, reads come from
// the inflater goroutine through buf, bounded by the read deadline.
type deflateConn struct {
	raw  net.Conn
	wmu  sync.Mutex    // serialises Write (the flate.Writer is not safe for concurrent use)
	zw   *flate.Writer // writes into out
	out  bytes.Buffer  // the deflated bytes of one Write, sent to raw in one write
	werr error         // the first failed write to raw: the stream is broken from there on

	mu       sync.Mutex
	room     *sync.Cond // the inflater waits on it while buf is full
	buf      []byte     // inflated and not yet read
	err      error      // the end of the inflated stream, returned once buf is empty
	closed   bool
	deadline time.Time     // read deadline of the inflated stream
	wake     chan struct{} // capacity 1: data, an error, Close or a new deadline
}

// notify wakes a Read that waits.
func (d *deflateConn) notify() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Read returns inflated bytes; it waits until some arrive, the stream ends or the read
// deadline passes (a net.Error whose Timeout is true, wrapping os.ErrDeadlineExceeded).
func (d *deflateConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		d.mu.Lock()
		switch {
		case d.closed:
			d.mu.Unlock()
			return 0, net.ErrClosed
		case len(d.buf) > 0:
			n := copy(p, d.buf)
			d.buf = d.buf[n:]
			d.room.Signal()
			d.mu.Unlock()
			return n, nil
		case d.err != nil:
			err := d.err
			d.mu.Unlock()
			return 0, err
		}
		dl := d.deadline
		d.mu.Unlock()
		if dl.IsZero() {
			<-d.wake
			continue
		}
		wait := time.Until(dl)
		if wait <= 0 {
			return 0, &net.OpError{Op: "read", Net: d.raw.LocalAddr().Network(),
				Source: d.raw.LocalAddr(), Addr: d.raw.RemoteAddr(), Err: os.ErrDeadlineExceeded}
		}
		tm := time.NewTimer(wait)
		select {
		case <-d.wake:
		case <-tm.C:
		}
		tm.Stop()
	}
}

// inflate runs on its own goroutine: it inflates the server's stream from src (the reader
// that read the 206, holding the first compressed bytes) into buf until the stream ends.
func (d *deflateConn) inflate(src *bufio.Reader) {
	err := inflate.Run(src, d.put)
	var ce flate.CorruptInputError
	if errors.As(err, &ce) {
		err = fmt.Errorf("client: %w", err)
	}
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
	d.notify()
}

// put appends inflated bytes to buf; it waits while buf is full and fails once d is closed.
func (d *deflateConn) put(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.buf) >= pendingMax && !d.closed {
		d.room.Wait()
	}
	if d.closed {
		return net.ErrClosed
	}
	d.buf = append(d.buf, b...)
	d.notify()
	return nil
}

// Write deflates p, flushes the compressor (sync flush) and writes the result to the socket
// in one write, so the server can inflate all of p at once; it returns len(p) on success.
// compress/flate's Writer on its own writes a short input as five pieces (stored block
// header, LEN/NLEN, the data, then the empty stored block of the flush in two): the server
// could decode the whole command from the third piece, answer and close, and the write of
// the fourth would fail although the answer was waiting. After a failed write the deflate
// stream is broken (the server lacks bytes the compressor already counted), so every later
// Write returns that error.
func (d *deflateConn) Write(p []byte) (int, error) {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if d.werr != nil {
		return 0, d.werr
	}
	d.out.Reset()
	if _, err := d.zw.Write(p); err != nil { // into d.out: cannot fail
		return 0, err
	}
	if err := d.zw.Flush(); err != nil {
		return 0, err
	}
	if _, err := d.raw.Write(d.out.Bytes()); err != nil {
		d.werr = err
		return 0, err
	}
	return len(p), nil
}

// Close closes the socket, which ends the inflater goroutine; safe to call more than once.
func (d *deflateConn) Close() error {
	d.mu.Lock()
	d.closed = true
	d.room.Broadcast()
	d.mu.Unlock()
	d.notify()
	return d.raw.Close()
}

func (d *deflateConn) LocalAddr() net.Addr  { return d.raw.LocalAddr() }
func (d *deflateConn) RemoteAddr() net.Addr { return d.raw.RemoteAddr() }

// SetDeadline sets the read deadline of the inflated stream and the socket's write deadline.
func (d *deflateConn) SetDeadline(t time.Time) error {
	_ = d.SetReadDeadline(t)
	return d.raw.SetWriteDeadline(t)
}

// SetReadDeadline bounds Read of the inflated stream; the socket itself is read without one.
func (d *deflateConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadline = t
	d.mu.Unlock()
	d.notify()
	return nil
}

func (d *deflateConn) SetWriteDeadline(t time.Time) error { return d.raw.SetWriteDeadline(t) }

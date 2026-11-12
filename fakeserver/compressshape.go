package fakeserver

import (
	"bytes"
	"compress/flate"
	"math/bits"
	"slices"
	"sort"
)

// CompressFlush is how the compressed output stream is flushed at the end of every write.
type CompressFlush int

const (
	// FlushSync ends every write with a sync flush (zlib's Z_SYNC_FLUSH): an empty stored
	// block, the marker 00 00 FF FF on a byte boundary. The default.
	FlushSync CompressFlush = iota
	// FlushPartial ends every write with zlib's Z_PARTIAL_FLUSH, as INN's nnrpd does: an empty
	// fixed-Huffman block (10 bits), of which only the whole bytes are sent. The last 0 to 7
	// bits stay in the compressor until the next write; the data before them is complete.
	// The data before them is complete: the held bits are only the tail of the empty block's
	// end-of-block code, which is zeros. There is no 00 00 FF FF marker in the stream. The data are literals in fixed-Huffman
	// blocks, one per write (no matches), so the output is larger than FlushSync's. A client
	// that waits for a stored block, or for the end of the block it is in, hangs on this
	// stream, as INN's clients must not.
	FlushPartial
	// FlushFinish ends the deflate stream (a final block) with the first write after the 206:
	// that write is deflated and the final empty block follows in the same socket write. Later
	// writes are plain bytes on the socket, after the end of the stream (the kit still inflates
	// what the client sends). CompressShape.AfterEnd adds bytes behind the final block.
	FlushFinish
)

// CompressShape shapes what the fake server sends on a COMPRESS DEFLATE connection (RFC 8054),
// for client tests: StoreOptions.CompressShape, PeerOptions.CompressShape, Step.CompressShape,
// Conn.StartCompressShaped. The zero value is the default: every write sync-flushed and sent
// in one socket write.
type CompressShape struct {
	// Flush is the flush at the end of each write, see CompressFlush.
	Flush CompressFlush

	// Split cuts the compressed bytes of each write (reply, Faults piece) into separate socket
	// writes: At holds offsets from the start of the write, a negative offset counts from its
	// end (with FlushSync -1 .. -3 cut inside the sync marker 00 00 FF FF, -4 before it, and
	// with FlushPartial -1 cuts the empty block's last byte), Size cuts a piece every Size
	// bytes when At is empty, Gap pauses after each piece but the last, bounded by the
	// server's Deadline like Faults.Split. A client that reads between the pieces sees
	// compressed bytes that end mid-block or mid-marker.
	Split Split

	// EmptyFlushWith206 writes an empty flush block (the marker of Flush) right after the CRLF
	// of the "206" in the same socket write as the 206 line, as a server that flushes its
	// compressor when it switches it on. A client must keep those bytes for its inflater. The
	// 206 is written by the kit's handlers (Store, Peer with Compress or CompressOutputOnly,
	// Script step with Compress); a Handler that writes its own 206 cannot use this. Faults on
	// the COMPRESS reply (DropAfterBytes, Split offsets) count these bytes too.
	EmptyFlushWith206 bool
	// With206 is deflated and flushed like a reply and written in the same socket write as the
	// 206 (it ends with its own flush, so EmptyFlushWith206 adds nothing then): compressed
	// payload pipelined behind the 206, for a client that reads past the 206 line.
	With206 []byte

	// AfterEnd is written in the same socket write as the final block, with FlushFinish:
	// trailing bytes after the end of the stream (junk, or plain text).
	AfterEnd []byte
}

func (s CompressShape) pipelined() bool { return s.EmptyFlushWith206 || len(s.With206) > 0 }

// cuts returns the sorted offsets in (0, n) where a write of n bytes is cut.
func (s Split) cuts(n int) []int {
	var out []int
	for _, a := range s.At {
		if a < 0 {
			a += n
		}
		if a > 0 && a < n {
			out = append(out, a)
		}
	}
	sort.Ints(out)
	out = slices.Compact(out)
	if len(s.At) == 0 && s.Size > 0 {
		for k := s.Size; k < n; k += s.Size {
			out = append(out, k)
		}
	}
	return out
}

// encoder deflates the output of a compressed connection as its shape says. Not safe for
// concurrent use (one connection goroutine writes).
type encoder struct {
	shape CompressShape
	zw    *flate.Writer // FlushSync and FlushFinish; writes into out
	out   bytes.Buffer
	bw    bitWriter // FlushPartial
	ended bool      // FlushFinish: the stream is over
}

func newEncoder(s CompressShape) (*encoder, error) {
	e := &encoder{shape: s}
	if s.Flush != FlushPartial {
		zw, err := flate.NewWriter(&e.out, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		e.zw = zw
	}
	return e, nil
}

// flushData deflates p and flushes (sync, or partial for FlushPartial; FlushFinish counts as
// sync here). An empty p is an empty flush block. The result is valid until the next call.
func (e *encoder) flushData(p []byte) ([]byte, error) {
	if e.shape.Flush == FlushPartial {
		return e.partial(p), nil
	}
	e.out.Reset()
	if _, err := e.zw.Write(p); err != nil { // into e.out: cannot fail
		return nil, err
	}
	if err := e.zw.Flush(); err != nil {
		return nil, err
	}
	return e.out.Bytes(), nil
}

// write returns the bytes to send for a write of p: deflated and flushed, or with FlushFinish
// the end of the stream (once; then p itself).
func (e *encoder) write(p []byte) ([]byte, error) {
	if e.ended {
		return p, nil
	}
	if e.shape.Flush != FlushFinish {
		return e.flushData(p)
	}
	e.out.Reset()
	if _, err := e.zw.Write(p); err != nil {
		return nil, err
	}
	if err := e.zw.Close(); err != nil {
		return nil, err
	}
	e.ended = true
	return append(e.out.Bytes(), e.shape.AfterEnd...), nil
}

// partial is zlib's Z_PARTIAL_FLUSH (deflate.c: FLUSH_BLOCK, then _tr_align): the data as one
// fixed-Huffman block of literals when there are any, then an empty fixed-Huffman block (3
// header bits, the 7-bit end-of-block code), and only the whole bytes are sent. Since zlib
// 1.2.5.2 that is all: older versions added a second empty block when fewer than 9 bits of
// the last block's end code had been sent.
func (e *encoder) partial(p []byte) []byte {
	w := &e.bw
	if len(p) > 0 {
		w.block(p)
	}
	w.block(nil)
	return w.take()
}

// bitWriter writes DEFLATE bits, the first one in the lowest bit of a byte (RFC 1951 3.1.1).
type bitWriter struct {
	out []byte
	acc uint32
	n   uint // bits in acc, fewer than 8 after put
}

func (w *bitWriter) put(v uint32, n uint) {
	w.acc |= v << w.n
	w.n += n
	for w.n >= 8 {
		w.out = append(w.out, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

// code writes an n-bit Huffman code, which goes most significant bit first.
func (w *bitWriter) code(v uint32, n uint) { w.put(bits.Reverse32(v)>>(32-n), n) }

// block writes a non-final fixed-Huffman block (BFINAL 0, BTYPE 01) of the literals p and the
// end-of-block code (RFC 1951 3.2.6: literals 0-143 are 8 bits from 00110000, 144-255 are 9
// bits from 110010000, the end of block is 7 zero bits).
func (w *bitWriter) block(p []byte) {
	w.put(2, 3) // BFINAL 0 in the lowest bit, BTYPE 01 above it
	for _, b := range p {
		if b < 144 {
			w.code(0x30+uint32(b), 8)
		} else {
			w.code(0x190+uint32(b)-144, 9)
		}
	}
	w.code(0, 7)
}

// take returns the whole bytes written so far; the last bits stay.
func (w *bitWriter) take() []byte {
	b := w.out
	w.out = nil
	return b
}

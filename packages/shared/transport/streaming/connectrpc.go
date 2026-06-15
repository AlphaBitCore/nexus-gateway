package streaming

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ConnectRPCFrameExtractor is called with each decoded Connect-RPC frame
// payload and returns the human-readable text it contains (empty if none).
type ConnectRPCFrameExtractor func(framePayload []byte) string

// MaxConnectRPCFrameLen caps a single Connect-RPC frame payload. The header's
// 4-byte length field is untrusted input — captured bodies that are not
// actually Connect-framed (or are truncated mid-frame) can spell lengths up
// to 4GB, and allocating before validation lets a few-hundred-byte body force
// a multi-GB allocation. Real chat frames are text deltas in the KB range;
// 16MB leaves generous headroom while keeping the worst-case allocation safe.
const MaxConnectRPCFrameLen = 16 << 20

// ErrConnectRPCFrameTooLarge is returned when a frame header declares a
// payload above MaxConnectRPCFrameLen. Callers treat it like any other
// framing error: stop walking, fall back to non-framed handling.
var ErrConnectRPCFrameTooLarge = errors.New("connect-rpc frame length exceeds cap")

// Connect-RPC envelope flag bits. The flags byte is a bitset: the
// least-significant bit marks a per-message gzip-compressed payload and the
// next bit marks the end-of-stream (trailer) message. They are independent —
// a single frame is never both, but both can appear across one stream (Cursor
// /agent.v1.AgentService/Run sends compressed data frames flagged 0x01 and a
// final empty trailer flagged 0x02).
const (
	ConnectFlagCompressed byte = 0x01
	ConnectFlagEndStream  byte = 0x02
)

// ReadConnectRPCFrame reads a single Connect-RPC envelope frame from r.
//
// Connect-RPC envelope format:
//
//	Byte  0:    flags  (bit 0x01 = compressed payload, bit 0x02 = end-of-stream)
//	Bytes 1–4:  message length (big-endian uint32)
//	Bytes 5+:   message bytes (gzip-compressed when the 0x01 flag is set)
//
// Returns the raw flags byte so callers can decompress (ConnectFlagCompressed)
// and detect the trailer (ConnectFlagEndStream) independently; the earlier
// design conflated compression with end-of-stream and stopped after the first
// compressed frame. Returns (flags, nil, nil) for zero-length frames and
// (0, nil, io.EOF) when the reader is cleanly exhausted.
func ReadConnectRPCFrame(r io.Reader) (flags byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	flags = hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:])
	if length == 0 {
		return flags, nil, nil
	}
	if length > MaxConnectRPCFrameLen {
		return flags, nil, ErrConnectRPCFrameTooLarge
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(r, payload)
	return flags, payload, err
}

// MaybeGunzipConnectFrame returns the decompressed payload when the frame's
// ConnectFlagCompressed bit is set, else the payload unchanged. Decompression
// is best-effort: a gzip error (corrupt or non-gzip body under the flag) yields
// the original bytes rather than aborting extraction, and the output is bounded
// by MaxConnectRPCFrameLen so a hostile gzip bomb cannot out-allocate the frame
// it rode in on.
func MaybeGunzipConnectFrame(flags byte, payload []byte) []byte {
	if flags&ConnectFlagCompressed == 0 || len(payload) == 0 {
		return payload
	}
	gr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return payload
	}
	defer func() { _ = gr.Close() }()
	dec, err := io.ReadAll(io.LimitReader(gr, MaxConnectRPCFrameLen))
	if err != nil {
		return payload
	}
	return dec
}

// PassthroughWithConnectRPCExtract relays Connect-RPC frames from upstream to
// client byte-for-byte while tee-ing payloads through extractor to accumulate
// response text for compliance audit.
//
// The byte relay is unmodified — the client receives the exact Connect-RPC
// wire encoding it expects. Content extraction runs asynchronously on a side
// goroutine so a slow or erroring extractor never blocks the relay.
//
// Frame payloads flagged ConnectFlagCompressed are gzip-decompressed before
// being passed to extractor (per-frame, the Connect-protocol-authoritative
// signal — a stream mixes compressed and uncompressed frames). The bytes
// forwarded to the client are NOT decompressed (the client owns decompression).
//
// Returns the full accumulated text extracted from all frames and any relay
// error. The relay error does not include extractor failures.
func PassthroughWithConnectRPCExtract(
	ctx context.Context,
	upstream io.Reader,
	client io.Writer,
	captureBuf *CappedBuffer,
	extractor ConnectRPCFrameExtractor,
) (accumulated string, err error) {
	flusher, canFlush := client.(http.Flusher)

	var dest = client
	if captureBuf != nil {
		dest = io.MultiWriter(client, captureBuf)
	}

	// Side pipe feeds relay bytes into the frame parser without blocking relay.
	pr, pw := io.Pipe()

	extractDone := make(chan string, 1)
	go func() {
		if extractor == nil {
			_, _ = io.Copy(io.Discard, pr)
			extractDone <- ""
			return
		}
		// strings.Builder so each frame's extracted text appends amortized
		// O(1); naive `all += ...` per frame is O(n²) over the stream
		// length.
		var all strings.Builder
		for {
			flags, payload, ferr := ReadConnectRPCFrame(pr)
			if ferr != nil {
				break
			}
			if len(payload) > 0 {
				all.WriteString(extractor(MaybeGunzipConnectFrame(flags, payload)))
			}
			if flags&ConnectFlagEndStream != 0 {
				break
			}
		}
		_, _ = io.Copy(io.Discard, pr)
		extractDone <- all.String()
	}()

	buf := make([]byte, 32*1024)
	var mainErr error
	for {
		if ctx.Err() != nil {
			mainErr = ctx.Err()
			break
		}
		n, readErr := upstream.Read(buf)
		if n > 0 {
			if _, writeErr := dest.Write(buf[:n]); writeErr != nil {
				mainErr = writeErr
				break
			}
			if canFlush {
				flusher.Flush()
			}
			// Best-effort feed to extractor; pipe errors are ignored so
			// a stuck goroutine never stalls the relay.
			_, _ = pw.Write(buf[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				mainErr = readErr
			}
			break
		}
	}

	_ = pw.Close()
	accumulated = <-extractDone
	return accumulated, mainErr
}

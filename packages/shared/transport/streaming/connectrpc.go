package streaming

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"net/http"
)

// ConnectRPCFrameExtractor is called with each decoded Connect-RPC frame
// payload and returns the human-readable text it contains (empty if none).
type ConnectRPCFrameExtractor func(framePayload []byte) string

// ReadConnectRPCFrame reads a single Connect-RPC envelope frame from r.
//
// Connect-RPC envelope format:
//
//	Byte  0:    flags  (bit 0 set = end-of-stream message)
//	Bytes 1–4:  message length (big-endian uint32)
//	Bytes 5+:   protobuf message bytes
//
// Returns (endOfStream=true, nil, nil) for zero-length end-of-stream frames.
// Returns (false, nil, io.EOF) when the reader is cleanly exhausted.
func ReadConnectRPCFrame(r io.Reader) (endOfStream bool, payload []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return false, nil, err
	}
	endOfStream = hdr[0]&0x01 != 0
	length := binary.BigEndian.Uint32(hdr[1:])
	if length == 0 {
		return endOfStream, nil, nil
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(r, payload)
	return endOfStream, payload, err
}

// PassthroughWithConnectRPCExtract relays Connect-RPC frames from upstream to
// client byte-for-byte while tee-ing payloads through extractor to accumulate
// response text for compliance audit.
//
// The byte relay is unmodified — the client receives the exact Connect-RPC
// wire encoding it expects. Content extraction runs asynchronously on a side
// goroutine so a slow or erroring extractor never blocks the relay.
//
// When payloadGzip is true the raw frame payloads are gzip-decompressed before
// being passed to extractor. The bytes forwarded to the client are NOT
// decompressed (the client owns decompression).
//
// Returns the full accumulated text extracted from all frames and any relay
// error. The relay error does not include extractor failures.
func PassthroughWithConnectRPCExtract(
	ctx context.Context,
	upstream io.Reader,
	client io.Writer,
	captureBuf *CappedBuffer,
	extractor ConnectRPCFrameExtractor,
	payloadGzip bool,
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
		var all string
		for {
			eos, payload, ferr := ReadConnectRPCFrame(pr)
			if ferr != nil {
				break
			}
			if len(payload) > 0 {
				p := payload
				if payloadGzip {
					if gr, gerr := gzip.NewReader(bytes.NewReader(p)); gerr == nil {
						if dec, rerr := io.ReadAll(gr); rerr == nil {
							p = dec
						}
						_ = gr.Close()
					}
				}
				all += extractor(p)
			}
			if eos {
				break
			}
		}
		_, _ = io.Copy(io.Discard, pr)
		extractDone <- all
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

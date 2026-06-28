package audit

// bodywire.go — the binary-wire form of Body, used by the binary audit MQ frame
// (see shared/transport/mq/binwire.go). It mirrors MarshalJSON / UnmarshalJSON
// but drops base64 entirely: an s2/zstd body travels as its RAW compressed frame
// and a binary/base64 body as its RAW bytes, so the Hub stores the bytes straight
// into the BYTEA column with no decode. That removes the gw-side base64 encode and
// the Hub-side base64 decode, and shrinks the message ~33% on the body.

import (
	"encoding/binary"
	"errors"
)

// Body binary kind discriminator (1 byte).
const (
	bwKindAbsent byte = 0
	bwKindInline byte = 1
	bwKindSpill  byte = 2
)

// Body binary encoding discriminator (1 byte, inline only).
const (
	bwEncRaw    byte = 0
	bwEncText   byte = 1
	bwEncBase64 byte = 2
	bwEncZstd   byte = 3
	bwEncS2     byte = 4
)

func encodingToByte(e BodyEncoding) byte {
	switch e {
	case EncodingText:
		return bwEncText
	case EncodingBase64:
		return bwEncBase64
	case EncodingZstd:
		return bwEncZstd
	case EncodingS2:
		return bwEncS2
	default: // EncodingRaw and "" (unset → raw)
		return bwEncRaw
	}
}

func byteToEncoding(b byte) (BodyEncoding, bool) {
	switch b {
	case bwEncRaw:
		return EncodingRaw, true
	case bwEncText:
		return EncodingText, true
	case bwEncBase64:
		return EncodingBase64, true
	case bwEncZstd:
		return EncodingZstd, true
	case bwEncS2:
		return EncodingS2, true
	default:
		return "", false
	}
}

// AppendBodyBinary appends the binary-wire encoding of b to dst and returns the
// grown slice. For an inline s2/zstd body it compresses InlineBytes (the ORIGINAL
// captured bytes — the same producer contract MarshalJSON assumes) into the raw
// frame and writes it without base64; for raw/text/base64 it writes InlineBytes
// verbatim. The Hub's ReadBodyBinary inverts this.
func AppendBodyBinary(dst []byte, b Body) []byte {
	switch b.Kind {
	case BodySpill:
		dst = append(dst, bwKindSpill)
		ref := b.SpillRef
		if ref == nil {
			ref = &SpillRef{}
		}
		dst = appendLenStr(dst, ref.Backend)
		dst = appendLenStr(dst, ref.Key)
		dst = binary.AppendVarint(dst, ref.Size)
		dst = appendLenStr(dst, ref.SHA256)
		dst = appendLenStr(dst, ref.ContentType)
		dst = appendBool(dst, ref.Truncated)
		dst = binary.AppendVarint(dst, b.SizeBytes)
		dst = appendBool(dst, b.Truncated)
		dst = appendLenStr(dst, b.ContentType)
		return dst
	case BodyInline:
		dst = append(dst, bwKindInline)
		dst = append(dst, encodingToByte(b.Encoding))
		switch b.Encoding {
		case EncodingS2:
			dst = appendLenPrefixed(dst, func(d []byte) []byte { return compressInlineS2Raw(d, b.InlineBytes) })
		case EncodingZstd:
			dst = appendLenPrefixed(dst, func(d []byte) []byte { return compressInlineZstdRaw(d, b.InlineBytes) })
		default:
			// raw / text / base64 → verbatim original bytes (no escape, no base64).
			dst = binary.AppendUvarint(dst, uint64(len(b.InlineBytes)))
			dst = append(dst, b.InlineBytes...)
		}
		dst = binary.AppendVarint(dst, b.SizeBytes)
		dst = appendBool(dst, b.Truncated)
		dst = appendLenStr(dst, b.ContentType)
		return dst
	default: // BodyAbsent and the zero Body
		return append(dst, bwKindAbsent)
	}
}

// ReadBodyBinary decodes one binary-wire Body from data and returns it plus the
// number of bytes consumed. For an s2/zstd body InlineBytes holds the RAW frame
// and inlineIsRawFrame is set so ColumnPayload stores it without a base64 decode.
func ReadBodyBinary(data []byte) (Body, int, error) {
	r := byteReader{b: data}
	kind, err := r.byte()
	if err != nil {
		return Body{}, 0, err
	}
	switch kind {
	case bwKindAbsent:
		return EmptyBody(), r.n, nil
	case bwKindInline:
		encByte, err := r.byte()
		if err != nil {
			return Body{}, 0, err
		}
		enc, ok := byteToEncoding(encByte)
		if !ok {
			return Body{}, 0, errors.New("audit.ReadBodyBinary: unknown inline encoding byte")
		}
		body, err := r.lenBytes()
		if err != nil {
			return Body{}, 0, err
		}
		size, err := r.varint()
		if err != nil {
			return Body{}, 0, err
		}
		trunc, err := r.bool()
		if err != nil {
			return Body{}, 0, err
		}
		ct, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		b := Body{
			Kind:        BodyInline,
			Encoding:    enc,
			InlineBytes: body,
			SizeBytes:   size,
			Truncated:   trunc,
			ContentType: ct,
		}
		if enc == EncodingS2 || enc == EncodingZstd {
			b.inlineIsRawFrame = true
		}
		return b, r.n, nil
	case bwKindSpill:
		backend, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		key, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		refSize, err := r.varint()
		if err != nil {
			return Body{}, 0, err
		}
		sha, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		refCT, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		refTrunc, err := r.bool()
		if err != nil {
			return Body{}, 0, err
		}
		size, err := r.varint()
		if err != nil {
			return Body{}, 0, err
		}
		trunc, err := r.bool()
		if err != nil {
			return Body{}, 0, err
		}
		ct, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		return Body{
			Kind: BodySpill,
			SpillRef: &SpillRef{
				Backend:     backend,
				Key:         key,
				Size:        refSize,
				SHA256:      sha,
				ContentType: refCT,
				Truncated:   refTrunc,
			},
			SizeBytes:   size,
			Truncated:   trunc,
			ContentType: ct,
		}, r.n, nil
	default:
		return Body{}, 0, errors.New("audit.ReadBodyBinary: unknown body kind byte")
	}
}

// ReadBodyBinaryMeta decodes one binary-wire Body's METADATA — Kind, Encoding,
// SizeBytes, Truncated, ContentType, and any spill ref — and returns it plus the
// bytes consumed. Framing is identical to ReadBodyBinary, but inline content
// bytes are skipped (advanced past via the length prefix), not copied, so
// InlineBytes stays nil. Read-only consumers that only need a body's
// presence/shape (e.g. the alerts engine's capture-failure rule) use this to
// avoid materializing the inline payload on the hot path.
func ReadBodyBinaryMeta(data []byte) (Body, int, error) {
	r := byteReader{b: data}
	kind, err := r.byte()
	if err != nil {
		return Body{}, 0, err
	}
	switch kind {
	case bwKindAbsent:
		return EmptyBody(), r.n, nil
	case bwKindInline:
		encByte, err := r.byte()
		if err != nil {
			return Body{}, 0, err
		}
		enc, ok := byteToEncoding(encByte)
		if !ok {
			return Body{}, 0, errors.New("audit.ReadBodyBinaryMeta: unknown inline encoding byte")
		}
		if err := r.skipLenBytes(); err != nil { // skip content; InlineBytes stays nil
			return Body{}, 0, err
		}
		size, err := r.varint()
		if err != nil {
			return Body{}, 0, err
		}
		trunc, err := r.bool()
		if err != nil {
			return Body{}, 0, err
		}
		ct, err := r.str()
		if err != nil {
			return Body{}, 0, err
		}
		return Body{
			Kind:        BodyInline,
			Encoding:    enc,
			SizeBytes:   size,
			Truncated:   trunc,
			ContentType: ct,
		}, r.n, nil
	case bwKindSpill:
		// Spill bodies carry no inline content — the full decode is already
		// metadata-only — so reuse it verbatim.
		return ReadBodyBinary(data)
	default:
		return Body{}, 0, errors.New("audit.ReadBodyBinaryMeta: unknown body kind byte")
	}
}

// --- small append helpers ---

func appendLenStr(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func appendBool(dst []byte, v bool) []byte {
	if v {
		return append(dst, 1)
	}
	return append(dst, 0)
}

// appendLenPrefixed reserves no length up front; it writes the body via fn into a
// scratch region of dst and back-patches the uvarint length. To keep it simple
// and allocation-light it compresses into a fresh tail, measures, then shifts —
// but since the uvarint length is variable we build the body first into a temp
// extension of dst and prepend the length.
func appendLenPrefixed(dst []byte, fn func([]byte) []byte) []byte {
	start := len(dst)
	body := fn(dst) // appends compressed frame after dst
	frameLen := len(body) - start
	// Insert the uvarint length at `start` by shifting the frame right.
	var lenbuf [binary.MaxVarintLen64]byte
	ln := binary.PutUvarint(lenbuf[:], uint64(frameLen))
	body = append(body, lenbuf[:ln]...) // grow by ln
	copy(body[start+ln:], body[start:start+frameLen])
	copy(body[start:], lenbuf[:ln])
	return body
}

// byteReader is a minimal sequential reader over a binary record/body.
type byteReader struct {
	b []byte
	n int
}

func (r *byteReader) byte() (byte, error) {
	if r.n >= len(r.b) {
		return 0, errors.New("audit.binwire: short read (byte)")
	}
	v := r.b[r.n]
	r.n++
	return v, nil
}

func (r *byteReader) bool() (bool, error) {
	v, err := r.byte()
	return v != 0, err
}

func (r *byteReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.n:])
	if n <= 0 {
		return 0, errors.New("audit.binwire: bad uvarint")
	}
	r.n += n
	return v, nil
}

func (r *byteReader) varint() (int64, error) {
	v, n := binary.Varint(r.b[r.n:])
	if n <= 0 {
		return 0, errors.New("audit.binwire: bad varint")
	}
	r.n += n
	return v, nil
}

// lenBytes reads a uvarint length then that many bytes, returning a COPY so the
// result outlives the source buffer (the NATS message is recycled after decode).
func (r *byteReader) lenBytes() ([]byte, error) {
	ln, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if ln == 0 {
		return nil, nil
	}
	// Unsigned bound: a length prefix >= 2^63 would make int(ln) negative and wrap
	// r.n+int(ln) past this check, then panic in make/slice. Compare against the
	// remaining bytes as uint64 so an oversized or corrupt length is a clean
	// short-read, never a panic that crashes the Hub consume goroutine. uvarint
	// leaves r.n <= len(r.b).
	if ln > uint64(len(r.b)-r.n) {
		return nil, errors.New("audit.binwire: short read (bytes)")
	}
	out := make([]byte, ln)
	copy(out, r.b[r.n:r.n+int(ln)])
	r.n += int(ln)
	return out, nil
}

// skipLenBytes reads a uvarint length prefix and advances past that many bytes
// without copying them, landing the reader exactly where lenBytes would. Same
// unsigned bounds check as lenBytes so a corrupt length is a clean short-read.
func (r *byteReader) skipLenBytes() error {
	ln, err := r.uvarint()
	if err != nil {
		return err
	}
	if ln > uint64(len(r.b)-r.n) {
		return errors.New("audit.binwire: short read (bytes)")
	}
	r.n += int(ln)
	return nil
}

func (r *byteReader) str() (string, error) {
	b, err := r.lenBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

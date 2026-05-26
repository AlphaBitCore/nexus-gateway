package streaming

import (
	"bytes"
	"testing"
)

// TestNewCappedBuffer_NonPositive_ReturnsNil — NewCappedBuffer(0/-1) yields
// nil so callers can no-op without an extra branch.
func TestNewCappedBuffer_NonPositive_ReturnsNil(t *testing.T) {
	for _, m := range []int{0, -1, -100} {
		if got := NewCappedBuffer(m); got != nil {
			t.Errorf("NewCappedBuffer(%d) = %v, want nil", m, got)
		}
	}
}

// TestCappedBuffer_Write_NilReceiver — Write on a nil receiver claims it
// consumed every byte so a MultiWriter wrapping a nil capture never errors.
func TestCappedBuffer_Write_NilReceiver(t *testing.T) {
	var b *CappedBuffer
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write on nil receiver err = %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if b.Bytes() != nil {
		t.Errorf("nil receiver Bytes = %q, want nil", b.Bytes())
	}
	if b.Truncated() {
		t.Error("nil receiver Truncated = true, want false")
	}
}

// TestCappedBuffer_Write_UnderCap — bytes within the cap are accumulated
// verbatim and Truncated stays false.
func TestCappedBuffer_Write_UnderCap(t *testing.T) {
	b := NewCappedBuffer(100)
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if !bytes.Equal(b.Bytes(), []byte("hello")) {
		t.Errorf("Bytes = %q, want hello", b.Bytes())
	}
	if b.Truncated() {
		t.Error("Truncated true after under-cap write")
	}
}

// TestCappedBuffer_Write_ExactCap — writing exactly cap bytes fills the
// buffer without marking truncated.
func TestCappedBuffer_Write_ExactCap(t *testing.T) {
	b := NewCappedBuffer(5)
	n, err := b.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write returned (%d, %v)", n, err)
	}
	if !bytes.Equal(b.Bytes(), []byte("hello")) {
		t.Errorf("Bytes = %q, want hello", b.Bytes())
	}
	if b.Truncated() {
		t.Error("Truncated true after exact-cap write")
	}
}

// TestCappedBuffer_Write_CrossesCap — first write straddles the cap; the
// prefix is kept, Truncated flips, and n still equals len(p) so MultiWriter
// does not abort.
func TestCappedBuffer_Write_CrossesCap(t *testing.T) {
	b := NewCappedBuffer(5)
	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if n != 11 {
		t.Errorf("n = %d, want 11 (full input length, not stored prefix)", n)
	}
	if !bytes.Equal(b.Bytes(), []byte("hello")) {
		t.Errorf("Bytes = %q, want hello (prefix only)", b.Bytes())
	}
	if !b.Truncated() {
		t.Error("Truncated = false, want true after overflow")
	}
}

// TestCappedBuffer_Write_AfterFull — second write when already at cap is a
// no-op stored but still reports full consumption.
func TestCappedBuffer_Write_AfterFull(t *testing.T) {
	b := NewCappedBuffer(3)
	if _, err := b.Write([]byte("abc")); err != nil {
		t.Fatalf("first write err = %v", err)
	}
	// Second write — remaining is 0.
	n, err := b.Write([]byte("def"))
	if err != nil {
		t.Fatalf("second write err = %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if !bytes.Equal(b.Bytes(), []byte("abc")) {
		t.Errorf("Bytes = %q, want abc (no overwrite)", b.Bytes())
	}
	if !b.Truncated() {
		t.Error("Truncated should be true after post-full write")
	}
}

// TestCappedBuffer_Write_MultipleWrites — sequential under-cap writes
// concatenate, then a final write that straddles the cap flips Truncated.
func TestCappedBuffer_Write_MultipleWrites(t *testing.T) {
	b := NewCappedBuffer(10)
	_, _ = b.Write([]byte("ab"))
	_, _ = b.Write([]byte("cde"))
	if b.Truncated() {
		t.Error("Truncated true after under-cap writes")
	}
	// Now overflow — only 5 more bytes fit.
	n, _ := b.Write([]byte("XYZ12345"))
	if n != 8 {
		t.Errorf("n = %d, want 8", n)
	}
	if !bytes.Equal(b.Bytes(), []byte("abcdeXYZ12")) {
		t.Errorf("Bytes = %q, want abcdeXYZ12", b.Bytes())
	}
	if !b.Truncated() {
		t.Error("Truncated should be true after overflow")
	}
}

// TestCappedBuffer_BytesAndTruncated_EmptyBuffer — pristine buffer returns
// empty slice and not-truncated.
func TestCappedBuffer_BytesAndTruncated_EmptyBuffer(t *testing.T) {
	b := NewCappedBuffer(10)
	if got := b.Bytes(); len(got) != 0 {
		t.Errorf("empty buf Bytes len = %d, want 0", len(got))
	}
	if b.Truncated() {
		t.Error("empty buf Truncated = true")
	}
}

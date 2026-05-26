package conn

import (
	"testing"
)

func TestBufferPool_GetPut(t *testing.T) {
	buf := GetBuffer()
	if buf == nil {
		t.Fatal("expected non-nil buffer from pool")
	}
	if len(*buf) != defaultBufferSize {
		t.Fatalf("expected buffer length %d, got %d", defaultBufferSize, len(*buf))
	}
	if cap(*buf) < defaultBufferSize {
		t.Fatalf("expected buffer capacity >= %d, got %d", defaultBufferSize, cap(*buf))
	}

	// Simulate partial use, then put back.
	*buf = (*buf)[:100]
	PutBuffer(buf)

	// Get again and verify length is restored.
	buf2 := GetBuffer()
	if len(*buf2) != defaultBufferSize {
		t.Fatalf("expected restored buffer length %d after PutBuffer, got %d", defaultBufferSize, len(*buf2))
	}
	PutBuffer(buf2)
}

func BenchmarkBufferPool(b *testing.B) {
	b.Run("pooled", func(b *testing.B) {
		for range b.N {
			buf := GetBuffer()
			_ = (*buf)[0]
			PutBuffer(buf)
		}
	})

	b.Run("fresh_alloc", func(b *testing.B) {
		for range b.N {
			buf := make([]byte, defaultBufferSize)
			_ = buf[0]
		}
	})
}

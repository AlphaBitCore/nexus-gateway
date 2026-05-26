package conn

import "sync"

const defaultBufferSize = 4096 // 4KB

// BufferPool manages reusable byte buffers to reduce GC pressure during
// bidirectional copy operations between client and upstream connections.
var BufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, defaultBufferSize)
		return &b
	},
}

// GetBuffer retrieves a buffer from the pool. The returned buffer has a
// length of defaultBufferSize (4096 bytes).
func GetBuffer() *[]byte {
	return BufferPool.Get().(*[]byte)
}

// PutBuffer returns a buffer to the pool after resetting its length to
// the default size. Callers must not use the buffer after calling PutBuffer.
func PutBuffer(b *[]byte) {
	*b = (*b)[:defaultBufferSize]
	BufferPool.Put(b)
}

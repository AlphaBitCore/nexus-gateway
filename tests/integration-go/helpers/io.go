package helpers

import (
	"bytes"
	"io"
)

// bytesReader returns nil for nil input so http.NewRequestWithContext
// sets Content-Length: 0 instead of an empty buffer (which some servers
// treat as a malformed empty-body POST).
func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

// readAll is a thin wrapper that lets the rest of the package stay free
// of net/http imports just for io.ReadAll.
func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

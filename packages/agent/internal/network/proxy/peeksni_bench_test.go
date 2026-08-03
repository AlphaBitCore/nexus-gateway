package proxy

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// PeekSNI runs on EVERY intercepted flow: the agent reads the TLS record
// header, then the ClientHello, to learn the SNI before deciding what to do
// with the connection. On endpoint hardware that per-flow cost is paid on a
// user's laptop for every connection their browser opens, so allocations here
// are worth counting.

// pipeConn feeds fixed bytes to PeekSNI over a real net.Conn so the deadline
// and io.ReadFull behaviour match production.
func pipeConn(b *testing.B, payload []byte) net.Conn {
	b.Helper()
	client, server := net.Pipe()
	go func() {
		_, _ = server.Write(payload)
		// Keep the pipe open; PeekSNI only reads what it needs.
	}()
	return client
}

func BenchmarkPeekSNI_ClientHello(b *testing.B) {
	hello := buildClientHello(b, "api.openai.com")

	// Sanity-check the fixture actually parses, so the benchmark cannot
	// silently measure an early-reject path.
	{
		c := pipeConn(b, hello)
		sni, peeked, err := PeekSNI(c, 2*time.Second)
		_ = c.Close()
		if err != nil {
			b.Fatalf("fixture PeekSNI: %v", err)
		}
		if sni != "api.openai.com" {
			b.Fatalf("fixture SNI = %q, want api.openai.com", sni)
		}
		if len(peeked) != len(hello) {
			b.Fatalf("peeked %d bytes, want %d — the replay buffer must carry the whole record", len(peeked), len(hello))
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c := pipeConn(b, hello)
		if _, _, err := PeekSNI(c, 2*time.Second); err != nil {
			b.Fatalf("PeekSNI: %v", err)
		}
		_ = c.Close()
	}
}

// BenchmarkPeekSNI_NonTLS covers the plaintext-HTTP / server-speaks-first
// reject path, which must stay cheap: the agent is in the host's outbound
// path and sees plenty of non-TLS flows.
func BenchmarkPeekSNI_NonTLS(b *testing.B) {
	payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")

	{
		c := pipeConn(b, payload)
		_, peeked, err := PeekSNI(c, 2*time.Second)
		_ = c.Close()
		if err == nil {
			b.Fatal("fixture: expected the non-TLS reject path")
		}
		if len(peeked) != 5 {
			b.Fatalf("non-TLS reject must still return the 5 peeked bytes for replay, got %d", len(peeked))
		}
		if !bytes.Equal(peeked, payload[:5]) {
			b.Fatalf("returned peek bytes do not match the wire: %q vs %q", peeked, payload[:5])
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c := pipeConn(b, payload)
		_, _, _ = PeekSNI(c, 2*time.Second)
		_ = c.Close()
	}
}

// BenchmarkExtractSNI isolates the parse from the I/O.
func BenchmarkExtractSNI(b *testing.B) {
	hello := buildClientHello(b, "api.anthropic.com")
	if got := ExtractSNI(hello); got != "api.anthropic.com" {
		b.Fatalf("fixture ExtractSNI = %q", got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = ExtractSNI(hello)
	}
}

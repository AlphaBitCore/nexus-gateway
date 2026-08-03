package proxy

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bodyread"
)

// readResponseBounded reads an upstream response body under a hard byte cap, sizing the
// buffer from Content-Length when the provider supplies one.
//
// Two cost arms, and which one runs is not the caller's choice. The gateway denylists
// Accept-Encoding on the request side so the Go transport owns gzip, and net/http reports
// ContentLength as -1 on every response it transparently gunzips — so the DECLARED-length arm
// only runs for uncompressed replies, and the undeclared arm is the common case. Any claim
// about this function that quotes one arm is describing the minority of production traffic.
//
// Measured per response body, go1.26 darwin/arm64, against the io.ReadAll call this replaced
// (go test -bench BenchmarkSweep -benchmem -benchtime=3000x -count=5 in
// shared/transport/bodyread; B/op and allocs/op, stable to a couple of bytes across runs):
//
//	body	 io.ReadAll	 Content-Length present	 Content-Length absent
//	8 KiB	 17,592 / 12	  9,504 /  2  (-46%)	 17,376 / 10  (-1.2%)
//	20 KB	 41,912 / 15	 20,512 /  2  (-51%)	 41,696 / 13  (-0.5%)
//	32 KiB	 80,824 / 17	 40,992 /  2  (-49%)	 80,608 / 15  (-0.3%)
//	64 KiB	138,169 / 18	 73,760 /  2  (-47%)	137,952 / 16  (-0.2%)
//	128 KiB	302,011 / 20	270,370 /  5  (-10%)	301,794 / 18  (-0.1%)
//
// So the win is the declared arm — a body whose length is known is read into one exactly-sized
// allocation with no copy, roughly halving the bytes — and the undeclared arm is a wash by
// design, because it reproduces io.ReadAll's own amortisation. That parity is the point rather
// than a shortfall: an earlier policy that doubled a single buffer bought a 2x cut in
// allocation COUNT on the undeclared arm and paid +55% to +89% in BYTES for it, on precisely
// the arm that carries the traffic. shared/transport/bodyread has a test that fails if the
// undeclared arm ever costs more than io.ReadAll again.
//
// Why sizing rather than a pooled buffer. A sync.Pool is cleared on every GC cycle, so on a
// LOW-RATE endpoint — which is exactly what these handlers are — the inter-arrival time can
// exceed the GC interval and most requests would miss the pool, paying its bookkeeping for
// none of its benefit. The buffer also escapes into the audit record, so it could not be
// returned to a pool without an ownership rule this call site does not have.
//
// The three response bodies these handlers read are small JSON despite the large caps — an
// STT transcript, a video job descriptor, a provider error body. The caps are DoS ceilings,
// not typical sizes; the multi-megabyte payloads on these endpoints are the REQUESTS, and the
// 2xx video artifact is streamed rather than buffered.
//
// The algorithm itself lives in shared/transport/bodyread, because the bumped path needs the
// identical read and the interaction between a declared length and the cap is subtle enough
// that two copies is the wrong number.
func readResponseBounded(resp *http.Response, maxBytes int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	return bodyread.Bounded(resp.Body, resp.ContentLength, maxBytes)
}

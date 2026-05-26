// Package proxy provides shared transparent proxy utilities (TCP relay, TLS SNI
// extraction, TLS MITM relay) used by the platform interception shims.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// upstreamControl returns the process-wide [net.Dialer.Control]
// callback installed via [nexushttp.SetGlobalDialControl], or nil.
// fetchUpstreamLeafCert and byteLevelFallback use it so the MITM
// upstream sockets are SO_MARK-stamped on Linux. On macOS / Windows
// the global stays nil and the dials run with default behaviour.
func upstreamControl() func(network, address string, c syscall.RawConn) error {
	return nexushttp.GlobalDialControl()
}

// defaultInspectBodyCap is the fallback request/response body cap (10 MB)
// used when the caller does not pass an explicit maxBodyBytes. MITMRelay
// accepts a runtime cap sourced from shared/payloadcapture.Store so
// admins can raise or lower the ceiling without a rebuild; the constant
// here only governs callers that pass 0 (legacy behaviour preserved).
const defaultInspectBodyCap int64 = 10 << 20

// InspectionResult is the outcome of inspecting a decrypted HTTP request or
// response.
type InspectionResult struct {
	Decision   string // "approve", "reject_hard", "block_soft"
	Reason     string
	ReasonCode string
	// ComplianceTags is the merged compliance tag set emitted by the hook
	// pipeline. The proxy layer accumulates across request and response
	// stages via mergeTagSets; the platform shim then propagates it into
	// FlowResult for audit.
	ComplianceTags []string

	// HookOutcome carries the aggregated hook pipeline outcome in the form
	// expected by traffic.FormatHookOutcome. Populated by the request
	// inspector from the compliance pipeline result; the response-side
	// outcome overrides this when a response inspector also ran. Used by
	// MITMRelay to inject X-Nexus-Hook onto the upstream response.
	HookOutcome traffic.HookOutcomeInput

	// Request-side LLM signals, populated by the traffic adapter's
	// DetectRequestMeta. Empty strings when the traffic did not match any
	// known provider adapter.
	Provider          string
	Model             string
	ApiKeyClass       string
	ApiKeyFingerprint string

	// Response-side usage populated when the MITM relay inspected the
	// response body (buffered JSON) or accumulated an SSE stream. Token
	// pointers are nil when usage was unavailable; UsageExtractionStatus
	// matches traffic.UsageStatus values.
	PromptTokens          *int
	CompletionTokens      *int
	UsageExtractionStatus string

	// Payload capture bytes forwarded by the inspector when the admin has
	// enabled the corresponding capture flag via
	// system_metadata["payload_capture.config"]. Bounded by the per-flow
	// inspectBodyCap (spill.perObjectCap, default 256 MiB); nil when
	// capture is disabled. Hub demuxes inline-vs-spill on receipt
	// against MaxInlineBodyBytes.
	PayloadRequest  []byte
	PayloadResponse []byte
}

// RequestInspector is called with the first HTTP request after TLS MITM to run
// the compliance hook pipeline and the LLM signal detector. Implementations
// must be safe for concurrent use.
type RequestInspector func(ctx context.Context, host, method, path string, headers http.Header, body []byte) InspectionResult

// ResponseInspector is called with the upstream HTTP response body after TLS
// MITM to run compliance hooks on the response stage. The usage argument
// carries the usage signals pre-computed by the proxy's response pipeline
// (from ResponseUsageDetector or a streaming accumulator) and is nil when no
// usage was computed. Implementations must be safe for concurrent use.
type ResponseInspector func(ctx context.Context, host, method, path string, body []byte, usage *traffic.UsageMeta) InspectionResult

// ResponseUsageDetector exposes the two usage-extraction entry points the
// proxy needs to populate traffic_event.prompt_tokens / completion_tokens /
// usage_extraction_status:
//
//   - NewUsageAccumulator: for text/event-stream responses, returns a
//     streaming.UsageAccumulator bound to the known provider/model. nil
//     means the provider has no registered extractor — the proxy falls back
//     to raw passthrough.
//   - ExtractResponseUsage: for buffered (non-streaming) response bodies,
//     runs the adapter's DetectResponseUsage and returns the resulting
//     UsageMeta.
//
// Implementations must be safe for concurrent use.
type ResponseUsageDetector interface {
	NewUsageAccumulator(provider, model string) streaming.UsageAccumulator
	ExtractResponseUsage(ctx context.Context, host, path string, resp *http.Response, body []byte) traffic.UsageMeta
}

// Relay copies data bidirectionally between two connections.
// Blocks until both directions complete. Returns bytes transferred.
func Relay(a, b net.Conn) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		aToB, _ = io.Copy(b, a)
		closeWrite(b)
	}()

	go func() {
		defer wg.Done()
		bToA, _ = io.Copy(a, b)
		closeWrite(a)
	}()

	wg.Wait()
	return
}

// closeWrite signals half-close on the write side for both plain TCP and TLS
// connections to prevent goroutine leaks when one relay direction finishes.
func closeWrite(conn net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := conn.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}

// PeekSNI reads the TLS ClientHello from a connection to extract the SNI
// hostname. Returns the peeked bytes which must be replayed to the actual
// TLS handshake via ReplayConn.
//
// Reads the 5-byte TLS record header first, then exactly recordLen more bytes
// via io.ReadFull to handle partial reads on slow connections correctly.
func PeekSNI(conn net.Conn, timeout time.Duration) (sni string, peeked []byte, err error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	// TLS record: type(1) + version(2) + length(2)
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", nil, fmt.Errorf("read TLS header: %w", err)
	}

	recordLen := int(header[3])<<8 | int(header[4])
	if recordLen < 1 || recordLen > 16384 {
		return "", header, fmt.Errorf("invalid TLS record length: %d", recordLen)
	}

	record := make([]byte, 5+recordLen)
	copy(record, header)
	if _, err := io.ReadFull(conn, record[5:]); err != nil {
		return "", record[:5], fmt.Errorf("read TLS record body: %w", err)
	}

	sni = ExtractSNI(record)
	return sni, record, nil
}

// ExtractSNI parses the SNI extension from a TLS ClientHello record.
// Returns "" if the data is not a valid ClientHello or has no SNI.
func ExtractSNI(hello []byte) string {
	// TLS record header: type(1) + version(2) + length(2)
	if len(hello) < 5 || hello[0] != 0x16 {
		return ""
	}
	recordLen := int(binary.BigEndian.Uint16(hello[3:5]))
	if len(hello) < 5+recordLen {
		return ""
	}
	data := hello[5 : 5+recordLen]

	// Handshake header: type(1) + length(3)
	if len(data) < 4 || data[0] != 0x01 {
		return ""
	}
	hsLen := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) < 4+hsLen {
		return ""
	}
	data = data[4 : 4+hsLen]

	// ClientHello: version(2) + random(32) = 34 bytes minimum
	if len(data) < 34 {
		return ""
	}
	pos := 34

	// Session ID (length-prefixed)
	if pos >= len(data) {
		return ""
	}
	pos += 1 + int(data[pos])

	// Cipher suites (2-byte length prefix)
	if pos+2 > len(data) {
		return ""
	}
	pos += 2 + int(binary.BigEndian.Uint16(data[pos:]))

	// Compression methods (1-byte length prefix)
	if pos >= len(data) {
		return ""
	}
	pos += 1 + int(data[pos])

	// Extensions (2-byte length prefix)
	if pos+2 > len(data) {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	extEnd := pos + extLen

	for pos+4 <= extEnd && pos+4 <= len(data) {
		extType := binary.BigEndian.Uint16(data[pos:])
		extDataLen := int(binary.BigEndian.Uint16(data[pos+2:]))
		pos += 4
		if pos+extDataLen > len(data) {
			break
		}
		if extType == 0x0000 { // SNI extension
			return parseSNIExtension(data[pos : pos+extDataLen])
		}
		pos += extDataLen
	}
	return ""
}

func parseSNIExtension(ext []byte) string {
	// SNI list: total_length(2) then entries: type(1) + name_length(2) + name
	if len(ext) < 5 {
		return ""
	}
	entryPos := 2 // skip list length
	if entryPos+3 > len(ext) {
		return ""
	}
	nameType := ext[entryPos]
	nameLen := int(binary.BigEndian.Uint16(ext[entryPos+1:]))
	entryPos += 3
	if nameType == 0 && entryPos+nameLen <= len(ext) {
		return string(ext[entryPos : entryPos+nameLen])
	}
	return ""
}

// MITMRelay performs a TLS MITM relay between a client and an upstream server:
//  1. Connects to the upstream server, performs a real TLS handshake
//  2. Generates a mimic certificate matching the server's cert
//  3. Performs a TLS handshake with the client using the mimic cert
//  4. (Optional) Inspects the first HTTP request via the inspector callback
//  5. (Optional) Inspects the first HTTP response: routes text/event-stream
//     through a streaming.UsageAccumulator, buffers JSON bodies ≤10MB for
//     compliance hooks + adapter.DetectResponseUsage, or relays raw bytes
//  6. Relays any remaining bidirectional traffic
//
// peekedClientHello contains bytes already read from the client (via PeekSNI)
// that must be replayed to the TLS handshake.
//
// If inspector is non-nil, the first HTTP request is parsed and passed to the
// inspector for compliance checking. A reject_hard decision sends HTTP 403 to
// the client and closes the connection. A block_soft decision logs a warning
// header but forwards the request. The inspection result is returned so the
// caller can record it in audit events.
//
// If respInspector is non-nil, the first HTTP response is parsed and passed
// through the response pipeline. Usage fields on the returned InspectionResult
// come from detector (SSE accumulator or buffered adapter call) when detector
// is non-nil; otherwise they stay empty.
//
// flowID is stamped onto the outbound request as the X-Nexus-Request-Id header
// so compliance-proxy and ai-gateway audit events can be correlated with this
// agent flow. Empty flowID skips header stamping (e.g. synthetic tests).
//
// maxBodyBytes bounds how much of the request/response body MITMRelay will
// buffer before truncating; the same cap applies to what the inspectors
// see, what is forwarded upstream, and what capture may stamp onto the
// audit event. 0 falls back to defaultInspectBodyCap (10 MB) so callers
// that do not yet wire payloadcapture.Store keep today's behaviour.
func MITMRelay(ctx context.Context, relayClient *relay.Client, clientConn net.Conn, peekedClientHello []byte, dstHost string, dstPort int, engine *agentTLS.Engine, inspector RequestInspector, respInspector ResponseInspector, detector ResponseUsageDetector, flowID string, maxBodyBytes int64) (bytesIn, bytesOut int64, inspection InspectionResult, err error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultInspectBodyCap
	}
	if relayClient == nil {
		return 0, 0, inspection, fmt.Errorf("MITMRelay: relayClient is nil")
	}

	// Step 1: One-shot upstream TLS dial purely to fetch the leaf cert
	// for mimicking. The connection is closed immediately; subsequent
	// outbound traffic flows through relayClient (per-host pooled).
	upstreamLeaf, err := fetchUpstreamLeafCert(ctx, dstHost, dstPort)
	if err != nil {
		return 0, 0, inspection, fmt.Errorf("fetch upstream cert %s:%d: %w", dstHost, dstPort, err)
	}

	// Step 2: Generate mimic leaf certificate
	leafCert, err := engine.IssueLeafCert(upstreamLeaf)
	if err != nil {
		return 0, 0, inspection, fmt.Errorf("issue leaf cert for %s: %w", dstHost, err)
	}
	tlsCert, err := tls.X509KeyPair(leafCert.CertPEM, leafCert.KeyPEM)
	if err != nil {
		return 0, 0, inspection, fmt.Errorf("create cert pair: %w", err)
	}

	// Step 3: TLS handshake with client using mimic cert. Pin MinVersion
	// at TLS 1.2 so the MITM channel cannot negotiate weaker than what
	// upstream would accept; bootstrap (internal/auth/bootstrap.go) already
	// pins TLS 1.3 on the Hub-control plane channel.
	//
	// ALPN negotiation: advertise ONLY http/1.1. Modern AI SDKs (Anthropic,
	// OpenAI, Cursor's Electron net stack) negotiate h2 by default when the
	// server offers it, but our per-request loop below uses
	// http.ReadRequest which is HTTP/1.1-only — h2 frames make ReadRequest
	// error and fall through to byteLevelFallback, which produces audit
	// rows with empty method/path/body (without this restriction, inspected
	// rows have Method=— Path=—). Restricting
	// ALPN to http/1.1 forces the client to downgrade — both Anthropic
	// SDK and Cursor handle this gracefully because the same SDK has to
	// support legacy HTTP/1.1-only proxies in enterprise. Future work is
	// adding real h2 support to MITMRelay is future work.
	// Build a request-scoped logger anchored on the canonical trace_id key
	// so every diag emit from this MITMRelay invocation carries the trace
	// correlation column automatically. The shared SlogSink (process-wide
	// slog handler chain) lifts this attr into DiagEvent.TraceID; Hub
	// persists it into thing_diag_event.trace_id. flow_id is the agent-
	// minted X-Nexus-Request-Id stamped onto the outbound request — same
	// value, two key names: flow_id is the agent-local debug field that
	// pairs with our `flowID` parameter for log greppability, trace_id is
	// the cross-service correlation column.
	logger := slog.With("flow_id", flowID, "trace_id", flowID)

	replayConn := NewReplayConn(clientConn, peekedClientHello)
	logger.Info("MITMRelay: TLS handshake start",
		"dst_host", dstHost, "dst_port", dstPort, "peeked_bytes", len(peekedClientHello))
	clientTLS := tls.Server(replayConn, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	if err := clientTLS.HandshakeContext(ctx); err != nil {
		logger.Warn("MITMRelay: TLS handshake FAILED",
			"dst_host", dstHost, "error", err)
		return 0, 0, inspection, fmt.Errorf("client TLS handshake for %s: %w", dstHost, err)
	}
	tlsState := clientTLS.ConnectionState()
	logger.Info("MITMRelay: TLS handshake OK",
		"dst_host", dstHost,
		"tls_version", tlsState.Version, "cipher", tlsState.CipherSuite,
		"alpn_negotiated", tlsState.NegotiatedProtocol)
	defer clientTLS.Close() //nolint:errcheck

	clientReader := bufio.NewReaderSize(clientTLS, 8192)

	// Step 4: Per-request loop on the (HTTP/1.1 keep-alive-capable)
	// client TLS connection. Each iteration parses one request, runs
	// the inspector, dispatches via relayClient.Do, and forwards the
	// response. The first iteration may fall back to byte-level relay
	// if the client speaks non-HTTP over TLS; later iterations exit
	// cleanly when the client closes or the read deadline expires.
	first := true
	for {
		// Per-request inactivity deadline. 2 minutes is generous for
		// idle keep-alive connections; an active request resets the
		// deadline once the request line + headers are read.
		_ = clientTLS.SetReadDeadline(time.Now().Add(2 * time.Minute))
		// Peek before ReadRequest so EOF / timeout exits the loop
		// without ReadRequest's "EOF" error noise.
		if _, perr := clientReader.Peek(1); perr != nil {
			break
		}
		_ = clientTLS.SetReadDeadline(time.Time{})

		var (
			result InspectionResult
			req    *http.Request
			rerr   error
		)
		if inspector != nil {
			result, req, rerr = inspectRequest(ctx, clientReader, clientTLS, dstHost, inspector, flowID, maxBodyBytes)
		} else {
			// No inspector wired: read the request directly so the loop
			// still drives client.Do per request.
			_ = clientTLS.SetReadDeadline(time.Now().Add(60 * time.Second))
			req, rerr = http.ReadRequest(clientReader)
			_ = clientTLS.SetReadDeadline(time.Time{})
			if rerr == nil && req.Body != nil {
				body, _ := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
				_ = req.Body.Close()
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				if flowID != "" {
					req.Header.Set("X-Nexus-Request-Id", flowID)
				}
			}
		}

		if rerr != nil {
			if first {
				logger.Warn("MITMRelay: first ReadRequest FAILED — falling back to byte-level relay (audit row will have empty method/path/body)",
					"dst_host", dstHost,
					"alpn_negotiated", clientTLS.ConnectionState().NegotiatedProtocol,
					"error", rerr,
					"diagnostic", "client likely speaks HTTP/2 over TLS but ALPN forced http/1.1 OR sent malformed HTTP/1.1 frames")
				return byteLevelFallback(ctx, clientTLS, clientReader, dstHost, dstPort)
			}
			logger.Debug("MITMRelay: per-request loop exit (subsequent request)",
				"dst_host", dstHost, "error", rerr)
			break
		}
		first = false
		logger.Info("MITMRelay: HTTP request parsed",
			"dst_host", dstHost,
			"method", req.Method, "path", req.URL.RequestURI(),
			"hook_decision", result.Decision, "body_size", req.ContentLength)

		if result.Decision == "reject_hard" {
			return bytesIn, bytesOut, result, nil
		}

		// Inbound edge: determine the nexus request id for this intercepted
		// request and seed it onto the per-request context so any outbound
		// httpclient call (notably relayClient.Do below) can log and, when
		// opted in, propagate it as x-nexus-request-id. The agent does not
		// currently mint per-request ids upstream of MITMRelay, so generate
		// one when the client did not supply it and stamp it on the request
		// header for the upstream service to observe.
		reqID := req.Header.Get("X-Nexus-Request-Id")
		if reqID == "" {
			reqID = uuid.New().String()
			req.Header.Set("X-Nexus-Request-Id", reqID)
		}
		reqCtx := nexushttp.WithRequestID(ctx, reqID)

		reqMethod := req.Method
		reqPath := req.URL.RequestURI()

		outURL := fmt.Sprintf("https://%s:%d%s", dstHost, dstPort, reqPath)
		outReq, oerr := http.NewRequestWithContext(reqCtx, req.Method, outURL, req.Body)
		if oerr != nil {
			return bytesIn, bytesOut, result, fmt.Errorf("build outbound request: %w", oerr)
		}
		for k, vv := range req.Header {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}
		outReq.ContentLength = req.ContentLength

		resp, dErr := relayClient.Do(outReq)
		if dErr != nil {
			_, _ = clientTLS.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			return bytesIn, bytesOut, result, fmt.Errorf("relay Do: %w", dErr)
		}

		// Build the per-response marker from request-side inspection state.
		// FlowID is already known from the outer MITMRelay parameter;
		// HookOutcome comes from the request inspector result (best effort:
		// if the response inspector also ran, its outcome is not yet known
		// at this point — it is stamped on the audit event via respResult but
		// not retroactively re-injected into headers that are already sent).
		marker := &AgentMarker{
			FlowID:      flowID,
			HookOutcome: result.HookOutcome,
		}

		// Time the response inspector + body read so the agent's audit row
		// carries response_hooks_ms. AddBreakdown stamps the per-request
		// PhaseSink which the platform code reads at flow completion.
		// Nil-safe when no sink is on ctx (test paths).
		respHookStart := time.Now()
		bIn, bOut, respResult, _ := inspectResponse(ctx, clientTLS, resp, dstHost, reqMethod, reqPath, result.Provider, result.Model, respInspector, detector, maxBodyBytes, marker)
		if ps := traffic.PhaseSinkFromContext(ctx); ps != nil {
			ps.AddBreakdown("response_hooks_ms", int(time.Since(respHookStart).Milliseconds()))
		}
		bytesIn += bIn
		bytesOut += bOut

		if respResult.Decision != "" {
			result.PromptTokens = respResult.PromptTokens
			result.CompletionTokens = respResult.CompletionTokens
			result.UsageExtractionStatus = respResult.UsageExtractionStatus
			result.PayloadResponse = respResult.PayloadResponse
			if respResult.Decision != "approve" {
				result.Decision = respResult.Decision
				result.Reason = respResult.Reason
				result.ReasonCode = respResult.ReasonCode
			}
			if len(respResult.ComplianceTags) > 0 {
				result.ComplianceTags = mergeTagSets(result.ComplianceTags, respResult.ComplianceTags)
			}
		}
		inspection = result

		if !shouldKeepAlive(req, resp) {
			break
		}
	}

	return bytesIn, bytesOut, inspection, nil
}

// fetchUpstreamLeafCert is a package-level seam wrapping
// realFetchUpstreamLeafCert. Production code (including the in-process
// MITMRelay call) goes through this indirection unchanged; tests
// substitute it with a stub returning a canned leaf certificate so the
// MITMRelay happy path is exercisable without dialing the real internet
// (whose chain would be validated against the OS root pool, blocking
// unit tests). Same pattern as tlsRandReader in agent/core/network/tls,
// randReader in pkce/secretstore/ssoenroll, etc. — production never
// reassigns; only `_test.go` callers swap it via the testHook helper
// below and restore on cleanup.
var fetchUpstreamLeafCert = realFetchUpstreamLeafCert

// realFetchUpstreamDialer is a package-level seam constructing the
// *tls.Dialer used by realFetchUpstreamLeafCert. Production wires it
// with default OS-root-pool verification; tests substitute it with a
// dialer that has InsecureSkipVerify=true so the rest of the function
// (post-dial parse + leaf-cert extraction) is exercisable against
// in-process self-signed test servers whose chain the OS pool would
// otherwise reject. Same seam pattern as fetchUpstreamLeafCert /
// byteLevelFallbackDial above.
var realFetchUpstreamDialer = func(dstHost string) *tls.Dialer {
	return &tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout: 10 * time.Second,
			Control: upstreamControl(),
		},
		Config: &tls.Config{ServerName: dstHost},
	}
}

// realFetchUpstreamLeafCert does a one-shot TLS dial purely to learn the
// leaf cert the client will pin to. The connection is closed
// immediately; subsequent traffic flows through relayClient.
func realFetchUpstreamLeafCert(ctx context.Context, dstHost string, dstPort int) (*x509.Certificate, error) {
	d := realFetchUpstreamDialer(dstHost)
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(dstHost, strconv.Itoa(dstPort)))
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return nil, fmt.Errorf("dial returned non-TLS connection")
	}
	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no peer certificates from %s", dstHost)
	}
	return certs[0], nil
}

// byteLevelFallbackDial is a package-level seam wrapping
// realByteLevelFallbackDial. Production behaviour is unchanged; tests
// substitute it with a stub that returns a paired pipe end (or a real
// httptest TLS server) so the byte-pump happy path is exercisable
// without dialing a real TLS upstream whose chain the OS root pool
// would reject. Same pattern as fetchUpstreamLeafCert above.
var byteLevelFallbackDial = realByteLevelFallbackDial

// realByteLevelFallbackDial performs the production TLS dial to the
// upstream for byteLevelFallback.
func realByteLevelFallbackDial(ctx context.Context, dstHost string, dstPort int) (net.Conn, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout: 10 * time.Second,
			Control: upstreamControl(),
		},
		Config: &tls.Config{ServerName: dstHost},
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(dstHost, strconv.Itoa(dstPort)))
}

// byteLevelFallback runs a one-shot tls.Dial to dstHost:dstPort and
// byte-pumps between clientTLS and the new connection. Used only when
// the first decrypted byte stream is not parseable HTTP. Buffered bytes
// already consumed from clientTLS during the failed parse are replayed
// first to the upstream.
func byteLevelFallback(ctx context.Context, clientTLS net.Conn, clientReader *bufio.Reader, dstHost string, dstPort int) (bytesIn, bytesOut int64, inspection InspectionResult, err error) {
	upstream, derr := byteLevelFallbackDial(ctx, dstHost, dstPort)
	if derr != nil {
		return 0, 0, inspection, fmt.Errorf("fallback dial: %w", derr)
	}
	defer upstream.Close() //nolint:errcheck

	if buffered := clientReader.Buffered(); buffered > 0 {
		buf := make([]byte, buffered)
		n, _ := clientReader.Read(buf)
		_, _ = upstream.Write(buf[:n])
		bytesOut += int64(n)
	}
	bIn, bOut := Relay(clientTLS, upstream)
	bytesIn += bIn
	bytesOut += bOut
	return bytesIn, bytesOut, inspection, nil
}

// shouldKeepAlive mirrors net/http server semantics: HTTP/1.1 default
// keep-alive unless Connection: close is set on either side. HTTP/1.0
// requires explicit "Connection: keep-alive".
func shouldKeepAlive(req *http.Request, resp *http.Response) bool {
	if strings.EqualFold(resp.Header.Get("Connection"), "close") {
		return false
	}
	if strings.EqualFold(req.Header.Get("Connection"), "close") {
		return false
	}
	if req.ProtoMajor == 1 && req.ProtoMinor == 0 {
		return strings.EqualFold(req.Header.Get("Connection"), "keep-alive")
	}
	return true
}

// inspectRequest reads one HTTP request from clientReader (which wraps
// clientTLS), runs the request inspector, and returns the parsed
// request with its body re-buffered so the caller can forward it via
// http.Client.Do.
//
// On reject_hard the function writes a 403 to the client and returns
// (result, req, nil); the caller should close the connection.
//
// Unlike the legacy inspectFirstRequest, this function does NOT write
// to any upstream connection — outbound transport is the caller's
// responsibility (typically relay.Client.Do). The clientReader is
// reused across calls so pipelined keep-alive requests on the same
// client TLS connection do not lose buffered bytes.
//
// flowID, when non-empty, is stamped as the X-Nexus-Request-Id header on
// the returned request so downstream services can correlate audit
// events with this agent flow.
func inspectRequest(ctx context.Context, clientReader *bufio.Reader, clientTLS net.Conn, dstHost string, inspector RequestInspector, flowID string, maxBodyBytes int64) (InspectionResult, *http.Request, error) {
	// Per-request inactivity deadline; cleared after the request line +
	// headers are read so a slow-but-progressing body upload is not
	// truncated.
	_ = clientTLS.SetReadDeadline(time.Now().Add(60 * time.Second))
	req, err := http.ReadRequest(clientReader)
	_ = clientTLS.SetReadDeadline(time.Time{})
	if err != nil {
		return InspectionResult{Decision: "approve"}, nil, fmt.Errorf("read request: %w", err)
	}

	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
		_ = req.Body.Close()
	}

	result := inspector(ctx, dstHost, req.Method, req.URL.RequestURI(), req.Header, body)

	if result.Decision == "reject_hard" {
		msg := result.Reason
		if msg == "" {
			msg = "blocked by compliance policy"
		}
		hookValue := traffic.FormatHookOutcome(result.HookOutcome)
		expose := strings.Join(traffic.ExposeHeaders, ", ")
		var respLines string
		if flowID != "" {
			respLines = fmt.Sprintf(
				"HTTP/1.1 403 Forbidden\r\n"+
					"Content-Type: text/plain; charset=utf-8\r\n"+
					"Content-Length: %d\r\n"+
					"Connection: close\r\n"+
					"X-Nexus-Via: agent\r\n"+
					"X-Nexus-Mode: mitm\r\n"+
					"X-Nexus-Hook: %s\r\n"+
					"X-Nexus-Request-Id: %s\r\n"+
					"Access-Control-Expose-Headers: %s\r\n"+
					"\r\n%s",
				len(msg), hookValue, flowID, expose, msg)
		} else {
			respLines = fmt.Sprintf(
				"HTTP/1.1 403 Forbidden\r\n"+
					"Content-Type: text/plain; charset=utf-8\r\n"+
					"Content-Length: %d\r\n"+
					"Connection: close\r\n"+
					"X-Nexus-Via: agent\r\n"+
					"X-Nexus-Mode: mitm\r\n"+
					"X-Nexus-Hook: %s\r\n"+
					"Access-Control-Expose-Headers: %s\r\n"+
					"\r\n%s",
				len(msg), hookValue, expose, msg)
		}
		_, _ = clientTLS.Write([]byte(respLines))
		return result, req, nil
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if flowID != "" {
		req.Header.Set("X-Nexus-Request-Id", flowID)
	}
	return result, req, nil
}

// inspectResponse forwards an already-parsed *http.Response (from a
// relay.Client.Do call) back to the client TLS connection. The
// dispatch on Content-Type is identical to the legacy
// inspectFirstResponse — SSE streaming via PassthroughWithAccumulator,
// non-streaming buffered up to maxBodyBytes for tier-1/tier-2 usage
// extraction and response-stage compliance hooks, oversized bodies
// raw-copied. The only difference is the response source: instead of
// http.ReadResponse over a bufio.Reader on serverTLS, the response is
// the value returned by relay.Client.Do.
//
// marker, when non-nil, has its x-nexus-agent-* headers injected into
// resp.Header before serializeResponseHead serializes them so the client
// receives the Nexus marker set. This is the agent's equivalent of
// responseio.Copy's HeaderHook — the injection point differs because the
// relay pipeline uses relay.Client.Do (which returns a pre-parsed
// *http.Response) rather than a raw byte stream.
//
// Returns bytes written to client / read from upstream for the
// response body, plus the merged InspectionResult. An error is returned
// only for fatal serialization failures (head render).
func inspectResponse(ctx context.Context, clientTLS net.Conn, resp *http.Response, dstHost, reqMethod, reqPath, provider, model string, respInspector ResponseInspector, detector ResponseUsageDetector, maxBodyBytes int64, marker *AgentMarker) (bytesIn, bytesOut int64, result InspectionResult, err error) {
	defer resp.Body.Close() //nolint:errcheck

	// Inject x-nexus-agent-* markers before serializing the response head so
	// they flow to the client. Placement here mirrors responseio.Copy's hook:
	// it fires after the upstream response is parsed but before any bytes are
	// written to the client, ensuring hop-by-hop filtering has already run on
	// the upstream's headers and our markers are not accidentally stripped.
	if marker != nil {
		marker.injectInto(resp.Header)
	}

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")

	head, err := serializeResponseHead(resp)
	if err != nil {
		return bytesIn, bytesOut, result, fmt.Errorf("serialize response head: %w", err)
	}
	if _, werr := clientTLS.Write(head); werr != nil {
		return bytesIn, bytesOut, result, nil
	}
	bytesOut += int64(len(head))

	var usage traffic.UsageMeta
	switch {
	case isSSE:
		// SSE flow (chunked_async semantics): bytes relay to the client
		// in real time, but a bounded io.MultiWriter buffer captures the
		// stream up to maxBodyBytes. After Passthrough returns we hand
		// the buffered bytes to respInspector — the inspector cannot
		// block bytes already on the wire, but its Decision lands on
		// the audit row so SIEM / alerting can trigger post-hoc on the
		// response content.
		var acc streaming.UsageAccumulator
		if detector != nil && provider != "" {
			acc = detector.NewUsageAccumulator(provider, model)
		}
		counter := &byteCounter{w: clientTLS}
		bodyBuf := &cappedBuffer{cap: maxBodyBytes}
		// MultiWriter splits the upstream stream into "what the client
		// gets" (counter → clientTLS) and "what we keep for audit"
		// (bodyBuf, capped to keep agent memory bounded).
		streamSink := io.MultiWriter(counter, bodyBuf)
		if acc != nil {
			_ = streaming.PassthroughWithAccumulator(ctx, resp.Body, streamSink, acc)
			finalizeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			usage = acc.Finalize(finalizeCtx)
			cancel()
		} else {
			_ = streaming.Passthrough(ctx, resp.Body, streamSink)
			usage = traffic.UsageMeta{Status: traffic.UsageStatusStreamingUnavailable}
		}
		bytesIn += counter.n
		bytesOut += counter.n

		// Run the response inspector against the captured (possibly
		// truncated) body so the audit row records the response-side
		// hook decision. Without this the row's response_hook_decision
		// stays NULL on every SSE flow, defeating the dual-pipeline
		// audit promise.
		if respInspector != nil {
			result = respInspector(ctx, dstHost, reqMethod, reqPath, bodyBuf.Bytes(), &usage)
			return bytesIn, bytesOut, result, nil
		}

	default:
		limited := io.LimitReader(resp.Body, maxBodyBytes+1)
		body, _ := io.ReadAll(limited)
		if int64(len(body)) > maxBodyBytes {
			// Oversized: write captured prefix + remaining body raw to
			// the client. Cannot run inspection on truncated content.
			if _, werr := clientTLS.Write(body); werr == nil {
				bytesOut += int64(len(body))
			}
			n, _ := io.Copy(clientTLS, resp.Body)
			bytesIn += int64(len(body)) + n
			bytesOut += n
			usage = traffic.UsageMeta{Status: traffic.UsageStatusStreamingUnavailable}
			break
		}
		if _, werr := clientTLS.Write(body); werr == nil {
			bytesOut += int64(len(body))
		}
		bytesIn += int64(len(body))
		if detector != nil {
			usage = detector.ExtractResponseUsage(ctx, dstHost, reqPath, resp, body)
		} else {
			usage = traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
		}
		if respInspector != nil {
			result = respInspector(ctx, dstHost, reqMethod, reqPath, body, &usage)
			return bytesIn, bytesOut, result, nil
		}
	}

	result = InspectionResult{
		Decision:              "approve",
		PromptTokens:          usage.PromptTokens,
		CompletionTokens:      usage.CompletionTokens,
		UsageExtractionStatus: string(usage.Status),
	}
	return bytesIn, bytesOut, result, nil
}

// byteCounter wraps an io.Writer and counts bytes successfully written. Used
// to measure response bytes going from upstream through our response pipeline
// back to the client for audit accounting.
type byteCounter struct {
	w io.Writer
	n int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Flush forwards to the underlying writer if it supports http.Flusher so
// PassthroughWithAccumulator can flush SSE frames in real time.
func (c *byteCounter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// cappedBuffer is an io.Writer that captures up to `cap` bytes for later
// inspection (SSE: feed the response-stage hook + audit emit a sampled
// body without holding unbounded memory on a long stream). Bytes beyond
// `cap` are silently dropped — the caller-visible Bytes() slice reflects
// whatever was retained.
type cappedBuffer struct {
	buf []byte
	cap int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.cap <= 0 {
		return len(p), nil
	}
	remaining := c.cap - int64(len(c.buf))
	if remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) <= remaining {
		c.buf = append(c.buf, p...)
		return len(p), nil
	}
	c.buf = append(c.buf, p[:remaining]...)
	return len(p), nil
}

// Bytes returns the captured bytes (possibly truncated). Safe for use
// after streaming completes.
func (c *cappedBuffer) Bytes() []byte { return c.buf }

// serializeResponseHead renders the HTTP status line + headers for resp up
// to (but not including) the body.
func serializeResponseHead(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	proto := resp.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	if _, err := fmt.Fprintf(&buf, "%s %s\r\n", proto, resp.Status); err != nil {
		return nil, err
	}
	if err := resp.Header.Write(&buf); err != nil {
		return nil, err
	}
	buf.WriteString("\r\n")
	return buf.Bytes(), nil
}

// ReplayConn wraps a net.Conn and prepends buffered data to reads.
// Used to replay peeked ClientHello bytes back through the TLS handshake.
type ReplayConn struct {
	net.Conn
	replay []byte
	pos    int
}

// NewReplayConn creates a connection that replays data before reading from conn.
func NewReplayConn(conn net.Conn, replay []byte) *ReplayConn {
	return &ReplayConn{Conn: conn, replay: replay}
}

func (c *ReplayConn) Read(b []byte) (int, error) {
	if c.pos < len(c.replay) {
		n := copy(b, c.replay[c.pos:])
		c.pos += n
		return n, nil
	}
	return c.Conn.Read(b)
}

// ParseCONNECT reads an HTTP CONNECT request from the connection using buffered
// I/O to handle partial TCP reads correctly. Returns the target host, port, and
// a wrapped connection that replays any buffered-but-unconsumed bytes (e.g. TLS
// ClientHello sent in the same TCP segment as the CONNECT request).
func ParseCONNECT(conn net.Conn, timeout time.Duration) (host string, port int, wrappedConn net.Conn, err error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", 0, nil, err
	}
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	reader := bufio.NewReaderSize(conn, 4096)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", 0, nil, fmt.Errorf("read CONNECT line: %w", err)
	}

	// Parse "CONNECT host:port HTTP/1.x\r\n"
	var method, target, proto string
	_, scanErr := fmt.Sscanf(line, "%s %s %s", &method, &target, &proto)
	if scanErr != nil || method != "CONNECT" {
		return "", 0, nil, fmt.Errorf("not a CONNECT request: %.40s", line)
	}

	// Drain remaining headers (terminated by empty line)
	for {
		hdr, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(hdr) == "" {
			break
		}
	}

	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, nil, fmt.Errorf("invalid CONNECT target %q: %w", target, err)
	}
	portNum := 443
	if _, err := fmt.Sscanf(p, "%d", &portNum); err != nil {
		return "", 0, nil, fmt.Errorf("invalid port %q: %w", p, err)
	}

	// Wrap the connection: if the bufio.Reader buffered extra bytes (e.g. TLS
	// ClientHello), they must be replayed before reading from the raw conn.
	if reader.Buffered() > 0 {
		buffered := make([]byte, reader.Buffered())
		if _, err := io.ReadFull(reader, buffered); err != nil {
			return "", 0, nil, fmt.Errorf("drain buffered bytes: %w", err)
		}
		wrappedConn = NewReplayConn(conn, buffered)
	} else {
		wrappedConn = conn
	}
	return h, portNum, wrappedConn, nil
}

// RespondCONNECT sends the HTTP 200 Connection Established response.
func RespondCONNECT(conn net.Conn) error {
	_, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	return err
}

// RejectCONNECT sends an HTTP 403 Forbidden response and closes the connection.
func RejectCONNECT(conn net.Conn) {
	_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")) // best-effort: client may already have disconnected on the reject path
}

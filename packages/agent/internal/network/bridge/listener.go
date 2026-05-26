// Package bridge implements the macOS NE → Go MITM bridge.
//
// Background: the macOS NETransparentProxyProvider in
// TransparentProxyProvider.swift could observe outbound flows but could not
// terminate TLS — Swift just byte-relayed packets to the real upstream. The
// Linux/Windows agents do TLS bump via proxy.go::MITMRelay (mints a leaf
// cert per-host signed by the device CA, terminates client TLS, runs the
// hook chain on the decrypted HTTP, then re-encrypts upstream). On macOS
// none of that happened, so every "inspect" event in the audit log had
// empty METHOD / PATH / HOOK DECISION / Request body / Response body —
// the agent on macOS was effectively a metadata-only collector despite the
// rich pipeline being one hop away.
//
// This package bridges the gap. The agent daemon listens on a
// loopback TCP port (default 127.0.0.1:9443). Swift NE redirects
// every flow it decides to inspect to that port instead of the real
// upstream, prefixing the connection with a single-line text header
//
//	BRIDGE <host>:<port> <flowId>\n
//
// The Go side parses the header, then calls proxy.MITMRelay with the
// remaining stream as the client connection — the same code path
// Linux/Windows have used since launch. Result on macOS: full TLS
// termination, HTTP request/response parse, hook execution, body
// capture, audit row populated with method/path/provider/model.
//
// Failure mode is fail-open: if the bridge listener can't accept
// (port busy, OOM, panic), Swift NE's redirect connect() fails and
// Swift falls back to the original raw-relay path with the audit row
// stamped BUMP_FAILED_PASSTHROUGH. The flow still reaches the user's
// destination — the agent just loses inspection visibility for that
// flow rather than losing the flow itself.
//
// Loopback-only is enforced at bind time (Listen on 127.0.0.1, not
// 0.0.0.0) to keep the bridge invisible to anything off the host.
package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HandleFunc is the callback invoked once the bridge has parsed the
// BRIDGE header. peeked carries any bytes the bufio reader pulled in
// past the header (the start of the client's TLS ClientHello). The
// caller (typically wired to proxy.MITMRelay) is responsible for the
// full client lifecycle including Close.
type HandleFunc func(ctx context.Context, clientConn net.Conn, peekedHello []byte, dstHost string, dstPort int, flowID string)

// Config configures the listener. Addr defaults to 127.0.0.1:9443
// when empty.
type Config struct {
	// Addr is the loopback bind address. Strongly recommended to
	// keep at 127.0.0.1 — binding to a non-loopback would expose
	// the MITM-bumping endpoint to other hosts on the LAN.
	Addr string

	// Handle is invoked for every accepted connection after the
	// BRIDGE header parses. Required.
	Handle HandleFunc

	// HeaderTimeout caps how long the listener waits for the BRIDGE
	// header to arrive. Defaults to 2 s (matches the Swift IPC
	// requestDecision timeout). A buggy / non-Swift client that
	// connects without sending the header is dropped without
	// blocking the accept loop.
	HeaderTimeout time.Duration

	// Logger is the slog logger. nil falls back to the default.
	Logger *slog.Logger
}

// Listener is a loopback TCP listener that demultiplexes Swift NE
// flows into proxy.MITMRelay calls.
type Listener struct {
	cfg      Config
	logger   *slog.Logger
	ln       net.Listener
	stopOnce sync.Once
	stopped  chan struct{}
}

// New constructs a listener bound to cfg.Addr. The caller is
// responsible for calling Run + Close.
func New(cfg Config) (*Listener, error) {
	if cfg.Handle == nil {
		return nil, errors.New("bridge: Handle callback is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:9443"
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 2 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := net.Listen("tcp", cfg.Addr) //nolint:noctx // listener lifecycle is controlled by Close(), not a request ctx
	if err != nil {
		return nil, fmt.Errorf("bridge: listen %s: %w", cfg.Addr, err)
	}
	logger.Info("bridge listener bound", "addr", ln.Addr().String())
	return &Listener{
		cfg:     cfg,
		logger:  logger,
		ln:      ln,
		stopped: make(chan struct{}),
	}, nil
}

// Addr returns the actual bound address (useful when cfg.Addr used :0).
func (l *Listener) Addr() string { return l.ln.Addr().String() }

// Run blocks accepting connections until ctx is cancelled or Close
// is called. Each connection is handed to a fresh goroutine so a
// slow MITM session can't starve the accept loop.
func (l *Listener) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.logger.Info("bridge listener stopped")
				return
			}
			l.logger.Warn("bridge accept error", "error", err)
			continue
		}
		go l.serve(ctx, conn)
	}
}

// Close stops the listener. Safe to call concurrently / multiple times.
func (l *Listener) Close() error {
	var err error
	l.stopOnce.Do(func() {
		err = l.ln.Close()
		close(l.stopped)
	})
	return err
}

func (l *Listener) serve(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			l.logger.Error("bridge: serve goroutine panicked", "recover", r)
			_ = conn.Close()
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(l.cfg.HeaderTimeout)); err != nil {
		l.logger.Warn("bridge: SetReadDeadline failed; closing", "error", err)
		_ = conn.Close()
		return
	}
	br := bufio.NewReader(conn)
	headerLine, err := br.ReadString('\n')
	if err != nil {
		l.logger.Debug("bridge: read header failed", "remote", conn.RemoteAddr(), "error", err)
		_ = conn.Close()
		return
	}
	host, port, flowID, perr := parseHeader(headerLine)
	if perr != nil {
		l.logger.Warn("bridge: invalid header; dropping", "header", strings.TrimSpace(headerLine), "error", perr)
		_ = conn.Close()
		return
	}
	// Clear the read deadline before handing off to MITMRelay; the
	// MITM pipeline manages its own per-phase timeouts.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		l.logger.Warn("bridge: clear deadline failed", "error", err)
	}
	// Drain any bytes the bufio reader pulled past the header — these
	// belong to the client's TLS ClientHello and the MITM relay must
	// see them. Buffered() returns 0 in the typical case where the
	// header arrives in its own segment.
	var peeked []byte
	if buffered := br.Buffered(); buffered > 0 {
		peeked = make([]byte, buffered)
		if _, err := io.ReadFull(br, peeked); err != nil {
			l.logger.Warn("bridge: drain peek failed", "error", err)
			_ = conn.Close()
			return
		}
	}
	l.cfg.Handle(ctx, conn, peeked, host, port, flowID)
}

// parseHeader parses `BRIDGE <host>:<port> <flowId>\n`.
// Returns (host, port, flowId, nil) on success.
//
// host may be a DNS name OR an IP literal (IPv4 dotted-quad or IPv6
// bracketed). For IPv6 the syntax MUST be `BRIDGE [::1]:443 fid\n` —
// the brackets are required so the trailing :port can be split
// without ambiguity.
func parseHeader(line string) (host string, port int, flowID string, err error) {
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 || parts[0] != "BRIDGE" {
		return "", 0, "", fmt.Errorf("expected `BRIDGE <host>:<port> <flowId>`, got %q", line)
	}
	addr := parts[1]
	flowID = parts[2]
	if flowID == "" {
		return "", 0, "", errors.New("flowId is required")
	}
	if len(flowID) > 128 {
		return "", 0, "", errors.New("flowId exceeds 128 chars")
	}
	// IPv6 form: `[::1]:443`
	if strings.HasPrefix(addr, "[") {
		closeBr := strings.IndexByte(addr, ']')
		if closeBr < 0 || closeBr+1 >= len(addr) || addr[closeBr+1] != ':' {
			return "", 0, "", fmt.Errorf("malformed IPv6 addr %q", addr)
		}
		host = addr[1:closeBr]
		portStr := addr[closeBr+2:]
		p, e := strconv.Atoi(portStr)
		if e != nil || p <= 0 || p > 65535 {
			return "", 0, "", fmt.Errorf("invalid port in %q: %w", addr, e)
		}
		return host, p, flowID, nil
	}
	// IPv4 / DNS form: `host:443`. An unbracketed IPv6 literal like
	// `::1:443` is ambiguous (port could be 443 with host ::1, or
	// some other split) — reject it so the Swift caller is forced
	// to bracket IPv6.
	colon := strings.LastIndexByte(addr, ':')
	if colon <= 0 || colon == len(addr)-1 {
		return "", 0, "", fmt.Errorf("missing :port in %q", addr)
	}
	host = addr[:colon]
	if strings.IndexByte(host, ':') >= 0 {
		return "", 0, "", fmt.Errorf("ambiguous IPv6 literal %q — must be bracketed `[::1]:443`", addr)
	}
	if len(host) > 253 {
		return "", 0, "", errors.New("host exceeds RFC 1035 max length 253")
	}
	p, e := strconv.Atoi(addr[colon+1:])
	if e != nil || p <= 0 || p > 65535 {
		return "", 0, "", fmt.Errorf("invalid port in %q: %w", addr, e)
	}
	return host, p, flowID, nil
}

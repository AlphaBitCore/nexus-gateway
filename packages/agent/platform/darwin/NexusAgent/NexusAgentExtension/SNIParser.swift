// SNIParser.swift — extract Server Name Indication from a TLS
// ClientHello sitting in the first bytes of a flow.
//
// Why this exists: NEAppProxyFlow.remoteHostname is nil for callers
// that pre-resolve DNS (Chrome, Cursor's Electron helpers, anything
// using DoH/DoT, native getaddrinfo callers). For those flows the
// only place we can recover the real hostname (e.g. "chatgpt.com",
// "api.anthropic.com") is the SNI field that the TLS client sends
// in its very first frame. We peek the first 4-8 KB of the flow,
// walk the ClientHello structure, and pull the ServerName.
//
// Pure stdlib byte-level parsing — no CryptoKit, no Network/TLS
// internals — so it stays compatible with the system extension's
// minimal entitlement set.
//
// References:
//   - RFC 5246 §7.4.1.2  (TLS 1.2 ClientHello)
//   - RFC 8446 §4.1.2     (TLS 1.3 ClientHello — same prefix shape)
//   - RFC 6066 §3         (Server Name Indication extension)

import Foundation

enum SNIParser {

    /// Returns the SNI hostname if the data buffer begins with a TLS
    /// ClientHello carrying a ServerName extension; nil otherwise.
    /// Tolerates short / malformed buffers — only returns a value
    /// when the full SNI structure parses cleanly.
    static func extractSNI(from data: Data) -> String? {
        let bytes = [UInt8](data)
        var p = 0

        // TLS record header (5 bytes): type(1) + version(2) + length(2).
        // type 0x16 = Handshake.
        guard bytes.count >= p + 5, bytes[p] == 0x16 else { return nil }
        // Skip record-layer length — we work over the contiguous buffer.
        p += 5

        // Handshake message header (4 bytes): type(1) + length(3).
        // type 0x01 = ClientHello.
        guard bytes.count >= p + 4, bytes[p] == 0x01 else { return nil }
        p += 4

        // ClientHello.client_version (2 bytes).
        guard bytes.count >= p + 2 else { return nil }
        p += 2

        // ClientHello.random (32 bytes).
        guard bytes.count >= p + 32 else { return nil }
        p += 32

        // ClientHello.session_id (1 length byte + variable).
        guard bytes.count >= p + 1 else { return nil }
        let sidLen = Int(bytes[p])
        p += 1
        guard bytes.count >= p + sidLen else { return nil }
        p += sidLen

        // cipher_suites (2-byte length + variable).
        guard bytes.count >= p + 2 else { return nil }
        let csLen = (Int(bytes[p]) << 8) | Int(bytes[p + 1])
        p += 2
        guard bytes.count >= p + csLen else { return nil }
        p += csLen

        // compression_methods (1-byte length + variable).
        guard bytes.count >= p + 1 else { return nil }
        let cmLen = Int(bytes[p])
        p += 1
        guard bytes.count >= p + cmLen else { return nil }
        p += cmLen

        // extensions (2-byte length + extensions[]).
        guard bytes.count >= p + 2 else { return nil }
        let extsLen = (Int(bytes[p]) << 8) | Int(bytes[p + 1])
        p += 2
        let extsEnd = p + extsLen
        guard bytes.count >= extsEnd else { return nil }

        // Walk extensions looking for SNI (type 0x0000).
        while p + 4 <= extsEnd {
            let extType = (UInt16(bytes[p]) << 8) | UInt16(bytes[p + 1])
            let extLen = (Int(bytes[p + 2]) << 8) | Int(bytes[p + 3])
            p += 4
            guard p + extLen <= extsEnd else { return nil }

            if extType == 0x0000 {
                // SNI extension body:
                //   server_name_list_length(2)
                //   server_name[] {
                //     name_type(1)         // 0x00 = host_name
                //     name_length(2)
                //     name(variable, UTF-8)
                //   }
                guard extLen >= 2 else { return nil }
                var q = p
                let listLen = (Int(bytes[q]) << 8) | Int(bytes[q + 1])
                q += 2
                let listEnd = min(q + listLen, p + extLen)
                while q + 3 <= listEnd {
                    let nameType = bytes[q]
                    let nameLen = (Int(bytes[q + 1]) << 8) | Int(bytes[q + 2])
                    q += 3
                    guard q + nameLen <= listEnd else { return nil }
                    if nameType == 0x00 && nameLen > 0 {
                        let host = String(bytes: Array(bytes[q..<q + nameLen]),
                                          encoding: .utf8)
                        // Reject obviously bogus names (control chars,
                        // NUL bytes) — the server_name extension carries
                        // ASCII hostnames per RFC 6066 §3.
                        if let h = host, !h.contains("\0") {
                            return h
                        }
                        return nil
                    }
                    q += nameLen
                }
                return nil
            }
            p += extLen
        }
        return nil
    }
}

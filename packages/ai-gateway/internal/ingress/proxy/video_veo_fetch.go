package proxy

// video_veo_fetch.go — the D-V5b provider-URI dereference for the Veo video
// leg: Veo returns the finished artifact only as a `video.uri` inside the LRO
// result (there is no OpenAI-style content endpoint), so the download handler
// fetches that URI and streams it through the same tee + hygiene relay.
//
// This is the ONE place in the product where the gateway dereferences a
// provider-supplied URL, and it is treated as untrusted egress (a compromised
// provider response is the threat model):
//   - scheme must be https, no userinfo, default port only;
//   - the host must match the built-in Google download host allow-list;
//   - the dialer runs the shared SSRF guard on every RESOLVED IP
//     (BlockPrivateDialControl — closes DNS rebinding to private/loopback/
//     link-local/metadata targets);
//   - every redirect hop re-validates scheme+host against the same rules and
//     STRIPS the API-key header (a redirect must never carry the credential
//     to another origin — Go forwards custom headers across redirects, so the
//     strip is explicit).
// No Veo credential configured ⇒ this code never runs.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sharedhttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// veoAPIHost is the Gemini API host — the ONLY host the API-key header is
// ever attached to. The storage host (below) authenticates via a signed-URL
// query token and must never receive the key.
const veoAPIHost = "generativelanguage.googleapis.com"

// veoDownloadHosts is the built-in allow-list for the Veo artifact URI. A
// host matches when it equals an entry or is a subdomain of one (label-
// anchored suffix — "evilgeneativelanguage.googleapis.com.attacker.tld" does
// not match). Veo serves from the Gemini API host and redirects to Google
// storage.
var veoDownloadHosts = []string{
	veoAPIHost,
	"storage.googleapis.com",
}

var errVeoHostNotAllowed = errors.New("veo artifact URI host is not on the download allow-list")

// veoURLAllowed validates one hop's URL: https, no userinfo, no explicit
// non-443 port, allow-listed host.
func veoURLAllowed(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("veo artifact URI must be https")
	}
	if u.User != nil {
		return fmt.Errorf("veo artifact URI must not carry userinfo")
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("veo artifact URI must use the default port")
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range veoDownloadHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return errVeoHostNotAllowed
}

// veoFetchClient is the dedicated egress client: SSRF-guarded dialer +
// redirect re-validation + credential strip. No client-level timeout — the
// artifact stream is bounded by the request context and the relay ceiling.
var veoFetchClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   sharedhttp.BlockPrivateDialControl,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("veo artifact fetch: too many redirects")
		}
		if err := veoURLAllowed(req.URL); err != nil {
			return fmt.Errorf("veo artifact fetch redirect refused: %w", err)
		}
		// The credential belongs ONLY to the initial allow-listed host.
		req.Header.Del("X-Goog-Api-Key")
		return nil
	},
}

// doFetchVeoArtifact is the handler's seam onto fetchVeoArtifact — a var so
// tests can stub the network egress while the guard logic keeps its own unit
// tests (the allow-list makes a real loopback upstream unreachable by
// design).
var doFetchVeoArtifact = fetchVeoArtifact

// buildVeoArtifactRequest validates the provider-supplied URI against the
// D-V5b front-door guard and builds the initial request with the credential
// attached ONLY when the host is the Gemini API host. Split out so the
// least-privilege key gating is unit-testable without a network dial.
func buildVeoArtifactRequest(ctx context.Context, rawURI, apiKey string) (*http.Request, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return nil, fmt.Errorf("veo artifact URI unparsable: %w", err)
	}
	if err := veoURLAllowed(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("veo artifact fetch build: %w", err)
	}
	// Least privilege: the credential is attached ONLY when the initial host
	// is the Gemini API host. A `video.uri` that points straight at
	// storage.googleapis.com (allow-listed, signed-URL-authenticated) must
	// not receive the plaintext key — and if veoDownloadHosts is ever
	// widened, the key still cannot ride to the new host.
	if apiKey != "" && strings.ToLower(u.Hostname()) == veoAPIHost {
		req.Header.Set("X-Goog-Api-Key", apiKey)
	}
	return req, nil
}

// fetchVeoArtifact dereferences the provider-supplied artifact URI under the
// full D-V5b guard. The API key is attached only to the initial request (the
// storage redirect carries its own signed query token).
func fetchVeoArtifact(ctx context.Context, rawURI, apiKey string) (*http.Response, error) {
	req, err := buildVeoArtifactRequest(ctx, rawURI, apiKey)
	if err != nil {
		return nil, err
	}
	return veoFetchClient.Do(req)
}

package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// emptyPayloadHash is the SHA-256 of the empty string, the standard
// x-amz-content-sha256 value for GET/HEAD requests carrying no body.
var emptyPayloadHash = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// SigV4 service names. The runtime endpoints (InvokeModel,
// InvokeModelWithResponseStream, Converse) live on the
// `bedrock-runtime.<region>.amazonaws.com` host and MUST be signed
// with the `bedrock-runtime` service. The control-plane endpoints
// (ListFoundationModels, model lifecycle) live on
// `bedrock.<region>.amazonaws.com` and use service `bedrock`.
// AWS rejects mismatched scopes with SignatureDoesNotMatch.
const (
	bedrockRuntimeService = "bedrock-runtime"
	bedrockControlService = "bedrock"
)

// Transport implements [provcore.Transport] for AWS Bedrock runtime.
// Auth is SigV4 signed with the access/secret key pair the Resolver
// surfaces via Extras["aws.accessKey"], Extras["aws.secretKey"],
// Extras["aws.sessionToken"] (optional). Region is read from
// Extras["aws.region"].
type Transport struct {
	client *http.Client
	probe  *http.Client
	signer *awsv4.Signer
	log    *slog.Logger
}

// NewTransport builds a Transport.
func NewTransport(log *slog.Logger) *Transport {
	if log == nil {
		log = slog.Default()
	}
	return &Transport{
		client: specutil.NewHTTPClient(),
		probe:  specutil.NewProbeClient(),
		signer: awsv4.NewSigner(),
		log:    log,
	}
}

// BuildURL constructs the Bedrock runtime URL.
//   - EndpointChatCompletions: /model/<modelId>/invoke (or invoke-with-response-stream for streaming)
//   - EndpointEmbeddings: /model/<modelId>/invoke (no streaming — Bedrock embedding
//     models do not support InvokeModelWithResponseStream)
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, stream bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		region := target.Get("aws.region")
		if region == "" {
			return "", fmt.Errorf("bedrock: missing BaseURL and aws.region")
		}
		base = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	}
	switch endpoint {
	case typology.WireShapeBedrockConverse:
		if target.ProviderModelID == "" {
			return "", fmt.Errorf("bedrock: missing ProviderModelID")
		}
		action := "invoke"
		if stream {
			action = "invoke-with-response-stream"
		}
		return fmt.Sprintf("%s/model/%s/%s", base, target.ProviderModelID, action), nil
	case typology.WireShapeBedrockEmbeddings:
		if target.ProviderModelID == "" {
			return "", fmt.Errorf("bedrock: missing ProviderModelID for embedding model")
		}
		// Bedrock embedding models (Titan, Cohere) use synchronous /invoke only —
		// InvokeModelWithResponseStream is not supported for embedding endpoints.
		return fmt.Sprintf("%s/model/%s/invoke", base, target.ProviderModelID), nil
	default:
		return "", fmt.Errorf("bedrock: only chat_completions and embeddings are supported; got %q", endpoint)
	}
}

// ApplyAuth is intentionally a no-op — SigV4 must be applied after
// the request body is finalised. We sign inside Do instead.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.Get("aws.accessKey") == "" || target.Get("aws.secretKey") == "" {
		return fmt.Errorf("bedrock: missing aws.accessKey / aws.secretKey")
	}
	if target.Get("aws.region") == "" {
		return fmt.Errorf("bedrock: missing aws.region")
	}
	r.Header.Set("content-type", "application/json")
	if accept := r.Header.Get("Accept"); accept == "" {
		// Streaming uses InvokeModelWithResponseStream which returns
		// AWS event-stream framing; non-streaming InvokeModel returns
		// plain JSON. Advertising the right Accept lets AWS short-
		// circuit content negotiation and matches what the runtime SDK
		// emits.
		if strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream") {
			r.Header.Set("Accept", "application/vnd.amazon.eventstream")
		} else {
			r.Header.Set("Accept", "application/json")
		}
	}
	r.Header.Set("X-Nexus-Bedrock-AccessKey", target.Get("aws.accessKey"))
	r.Header.Set("X-Nexus-Bedrock-SecretKey", target.Get("aws.secretKey"))
	r.Header.Set("X-Nexus-Bedrock-Region", target.Get("aws.region"))
	if st := target.Get("aws.sessionToken"); st != "" {
		r.Header.Set("X-Nexus-Bedrock-SessionToken", st)
	}
	return nil
}

// Do signs the request with SigV4 and sends it via the shared client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	accessKey := r.Header.Get("X-Nexus-Bedrock-AccessKey")
	secretKey := r.Header.Get("X-Nexus-Bedrock-SecretKey")
	region := r.Header.Get("X-Nexus-Bedrock-Region")
	sessionToken := r.Header.Get("X-Nexus-Bedrock-SessionToken")
	r.Header.Del("X-Nexus-Bedrock-AccessKey")
	r.Header.Del("X-Nexus-Bedrock-SecretKey")
	r.Header.Del("X-Nexus-Bedrock-Region")
	r.Header.Del("X-Nexus-Bedrock-SessionToken")
	if accessKey == "" || secretKey == "" || region == "" {
		return nil, fmt.Errorf("bedrock: internal signer state missing")
	}

	var payload []byte
	if r.Body != nil {
		var err error
		payload, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("bedrock: read body: %w", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(payload))
		r.ContentLength = int64(len(payload))
		sum := sha256.Sum256(payload)
		r.Header.Set("x-amz-content-sha256", hex.EncodeToString(sum[:]))
	} else {
		r.Header.Set("x-amz-content-sha256", emptyPayloadHash)
	}

	creds := aws.Credentials{
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		SessionToken:    sessionToken,
		Source:          "nexus-bedrock-transport",
	}
	if err := t.signer.SignHTTP(ctx, creds, r, r.Header.Get("x-amz-content-sha256"), bedrockRuntimeService, region, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("bedrock: sign: %w", err)
	}
	return t.client.Do(r.WithContext(ctx))
}

// Probe hits the Bedrock control-plane ListFoundationModels endpoint,
// which exists on the `bedrock` service in every region Bedrock is
// available in. When the target doesn't carry sufficient info we
// short-circuit with a static detail string.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	region := target.Get("aws.region")
	if region == "" {
		return &provcore.ProbeResult{OK: false, Detail: "missing aws.region"}, nil
	}
	if target.Get("aws.accessKey") == "" || target.Get("aws.secretKey") == "" {
		return &provcore.ProbeResult{OK: false, Detail: "missing aws.accessKey / aws.secretKey"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()

	url := fmt.Sprintf("https://bedrock.%s.amazonaws.com/foundation-models", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	req.Header.Set("x-amz-content-sha256", emptyPayloadHash)
	creds := aws.Credentials{
		AccessKeyID:     target.Get("aws.accessKey"),
		SecretAccessKey: target.Get("aws.secretKey"),
		SessionToken:    target.Get("aws.sessionToken"),
		Source:          "nexus-bedrock-transport",
	}
	if err := t.signer.SignHTTP(ctx, creds, req, emptyPayloadHash, bedrockControlService, region, time.Now().UTC()); err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	resp, err := t.probe.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: err.Error(), Err: err}, nil
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &provcore.ProbeResult{OK: true, LatencyMs: latency, Detail: "ok"}, nil
	}
	return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
}

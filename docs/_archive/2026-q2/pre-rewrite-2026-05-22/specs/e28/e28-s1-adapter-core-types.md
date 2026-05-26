# E28 — Story 1: Adapter core types and registry

## Context

Ground-floor story for the E28 redesign. Defines the new public contract (`Adapter`, `Request`, `Response`, `Probe`), the declarative composition unit (`AdapterSpec` + `Transport` + `SchemaCodec` + `StreamDecoder` + `ErrorNormalizer`), the internal target resolver (`CallTarget`, `TargetResolver`), and the registry. No provider logic yet — this is the scaffolding that stories s2 through s5 plug into. Deletes the current `BaseAdapter` hook-based pattern and `providers_util.go`'s `defaultTestConnectivity` in the same PR (pre-GA, no compat shim).

## User Story

**As an** AI Gateway maintainer,
**I want** a small set of well-named types that separate wire concerns from schema concerns,
**so that** provider implementations and callers (executor, smart router, AI Guard) share one non-leaky abstraction.

## Tasks

### 1. New type surface — `packages/ai-gateway/internal/providers/types.go`

Create the file with the following exports. Types only; no hooks, no receivers except as noted.

```go
// Format is the provider wire format. The set is one-to-one with the
// non-fallback IDs in shared/traffic/adapters (the `generic-jsonpath`
// traffic adapter has no provider-side counterpart and is intentionally
// absent here).
type Format string

const (
    FormatOpenAI      Format = "openai"
    FormatDeepSeek    Format = "deepseek"
    FormatGLM         Format = "glm"
    FormatAzureOpenAI Format = "azure-openai"
    FormatAnthropic   Format = "anthropic"
    FormatGemini      Format = "gemini"
    FormatMiniMax     Format = "minimax"
    FormatBedrock     Format = "bedrock"
    FormatVertex      Format = "vertex"
)

type Endpoint string

const (
    EndpointChatCompletions    Endpoint = "chat_completions"
    EndpointEmbeddings         Endpoint = "embeddings"
    EndpointModels             Endpoint = "models"
    EndpointCompletionsLegacy  Endpoint = "completions_legacy"
)

type CallTarget struct {
    ProviderID      string
    ProviderName    string   // stable slug: "openai", "anthropic", ...
    BaseURL         string   // no trailing slash
    APIKey          string   // plaintext after vault decrypt
    ProviderModelID string   // vendor's model ID (e.g. "claude-3-5-sonnet-20241022")
}

type Request struct {
    Endpoint    Endpoint
    BodyFormat  Format       // format of Body as received from ingress
    Body        []byte       // raw bytes; caller does not pre-parse
    Headers     http.Header  // filtered, safe-to-forward subset
    Stream      bool
    Target      CallTarget
}

type Response struct {
    StatusCode int
    Headers    http.Header
    Body       []byte                    // populated iff !Stream
    Stream     StreamSession             // populated iff Stream
    Usage      Usage
    BodyFormat Format                    // native format on wire from provider
}

type Usage struct {
    PromptTokens     *int
    CompletionTokens *int
    TotalTokens      *int
}

type StreamSession interface {
    // Next returns the next decoded chunk. io.EOF signals the end of stream.
    // RawBytes on each chunk is the provider-native SSE/NDJSON frame,
    // forwardable to the client without re-wrapping.
    Next(ctx context.Context) (Chunk, error)
    Close() error
}

type Chunk struct {
    Delta          string          // text delta, canonical UTF-8
    ToolCallDeltas []ToolCallDelta
    Usage          *Usage          // set when provider emits usage mid-stream or at end
    Done           bool            // terminal chunk
    RawBytes       []byte          // provider-native bytes (SSE frame incl. "data: " prefix, or NDJSON line)
    NativeEvent    string          // optional provider event name ("message_delta", etc.)
}

type ToolCallDelta struct {
    Index     int
    ID        string
    Name      string
    Arguments string
}

type ProbeResult struct {
    OK        bool
    LatencyMs int64
    Detail    string
    Err       error
}

type ProviderError struct {
    Status     int
    Code       string   // canonical: "invalid_request", "auth_failed", "rate_limited", "timeout", "upstream_error"
    Type       string   // provider's own type string, preserved for observability
    Message    string
    RetryAfter *time.Duration
    Raw        []byte   // provider error payload verbatim
}

func (e *ProviderError) Error() string { /* "<code>: <message>" */ }
```

### 2. Public `Adapter` interface — replace `adapter.go`

Overwrite `packages/ai-gateway/internal/providers/adapter.go` (delete existing content):

```go
type Adapter interface {
    // Format returned by the adapter on the wire (its native Format).
    Format() Format

    // Execute invokes the upstream provider. If req.Stream is true, the
    // Response.Stream is populated and Response.Body is nil.
    Execute(ctx context.Context, req Request) (*Response, error)

    // Probe is a health check. It must not mutate CallTarget. Cheap, idempotent.
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
}
```

No other exported methods. No `Type() string`. No `TestConnectivity`.

### 3. Composable component interfaces — `packages/ai-gateway/internal/providers/spec.go`

```go
type AdapterSpec struct {
    Format          Format
    Transport       Transport
    SchemaCodec     SchemaCodec
    StreamDecoder   StreamDecoder
    ErrorNormalizer ErrorNormalizer
}

// Transport owns URL construction, authentication, HTTP client, and Probe.
type Transport interface {
    // BuildURL composes BaseURL + endpoint-specific path + any provider-
    // specific URL segments (e.g. Azure's deployment in path, Gemini's model
    // in path with action). Never concatenate a caller-supplied path outside.
    BuildURL(target CallTarget, endpoint Endpoint, stream bool) (string, error)

    // ApplyAuth sets the right auth header(s) on the outbound request.
    ApplyAuth(r *http.Request, target CallTarget) error

    // Do executes the prepared request and returns the raw http.Response.
    // Implementations may wrap a shared *http.Client with provider-tuned
    // timeouts and transport options.
    Do(ctx context.Context, r *http.Request) (*http.Response, error)

    // Probe performs the adapter-specific health check.
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
}

// SchemaCodec converts between provider-native wire schema and canonical
// OpenAI shape. Callers may skip the codec entirely when BodyFormat already
// matches Format (passthrough fast path).
type SchemaCodec interface {
    // EncodeRequest converts a canonical OpenAI-shaped body into this
    // provider's native request body. If canonicalBody == nil it means the
    // caller already has native bytes (passthrough); return (nil, nil).
    EncodeRequest(endpoint Endpoint, canonicalBody []byte, target CallTarget) ([]byte, error)

    // DecodeResponse converts a native response body into canonical OpenAI
    // shape AND extracts Usage. Both outputs must be populated when possible.
    DecodeResponse(endpoint Endpoint, nativeBody []byte) (canonicalBody []byte, usage Usage, err error)
}

// StreamDecoder wraps the provider's streaming response body and produces
// a uniform Chunk stream.
type StreamDecoder interface {
    Open(r io.ReadCloser, endpoint Endpoint) (StreamSession, error)
}

// ErrorNormalizer converts a provider error response into ProviderError.
type ErrorNormalizer interface {
    Normalize(status int, headers http.Header, body []byte) *ProviderError
}
```

### 4. `specAdapter` — the glue — `packages/ai-gateway/internal/providers/spec_adapter.go`

Generic `Adapter` implementation composed from an `AdapterSpec`. Pseudocode:

```go
type specAdapter struct {
    spec AdapterSpec
    log  *slog.Logger
}

func NewSpecAdapter(spec AdapterSpec, log *slog.Logger) Adapter { /* validation, then return */ }

func (a *specAdapter) Format() Format { return a.spec.Format }

func (a *specAdapter) Execute(ctx context.Context, req Request) (*Response, error) {
    // 1. Choose body: passthrough iff req.BodyFormat == a.spec.Format, else encode.
    // 2. Build URL via Transport.BuildURL.
    // 3. Construct *http.Request; set headers from req.Headers filtered list + Content-Type per Format.
    // 4. Transport.ApplyAuth + Transport.Do.
    // 5. On non-2xx: ErrorNormalizer.Normalize → return *ProviderError.
    // 6. If req.Stream: StreamDecoder.Open → wrap in Response.Stream.
    // 7. Else: read body, SchemaCodec.DecodeResponse (skip if native == canonical), populate Usage.
    // 8. Return Response{BodyFormat: a.spec.Format, ...}.
}

func (a *specAdapter) Probe(ctx context.Context, target CallTarget) (*ProbeResult, error) {
    return a.spec.Transport.Probe(ctx, target)
}
```

### 5. Registry — rewrite `packages/ai-gateway/internal/providers/adapter_registry.go`

- Key by `Format` (not a loose `string`).
- `Get(format Format) (Adapter, bool)` — **no silent fallback** to openai; unknown returns `(nil, false)` and callers must 404 / 400.
- `Register(Adapter) error` — rejects duplicate formats; rejects a nil/invalid `AdapterSpec`.
- `RegisterBuiltins(...)` — constructs each of the **9** adapters (openai, deepseek, glm, azure-openai, anthropic, gemini, minimax, bedrock, vertex) via s2 and registers them. Fatal on error at startup. The set must equal `len(Format enum)` and equal the set returned by `shared/traffic/adapters.BuiltinTrafficAdapterIDs()` minus `{openai-compat→openai, generic-jsonpath}` (two ID-naming differences are bridged by a single helper in s5).

### 6. `CallTarget` resolver — new package `packages/ai-gateway/internal/providers/target/resolver.go`

Separate package to avoid an import cycle back from the `providers` package into `credential` store and provider catalog.

```go
type Resolver interface {
    Resolve(ctx context.Context, providerID, modelID string, hints ResolveHints) (providers.CallTarget, error)
}

type ResolveHints struct {
    PreferKeyID string        // optional pinning for AI Guard per-tenant keys
    Purpose     string        // "smart_router" | "ai_guard" | "egress"
}

type PgResolver struct {
    provStore ProviderStore       // existing provider/model catalog reader
    credStore CredentialStore     // vault-decrypting credential fetcher
    health    HealthTracker       // picks the best API key among healthy candidates
    log       *slog.Logger
}

func NewPgResolver(...) *PgResolver
func (r *PgResolver) Resolve(...) (providers.CallTarget, error)
```

Called by: target executor, smart routing, AI Guard `configured_provider` (story s3).

### 7. Deletions in this story

Delete these files/symbols (pre-GA, no compat shim):

- `packages/ai-gateway/internal/providers/base_adapter.go` — entire file.
- `packages/ai-gateway/internal/providers/base_stream.go` — superseded by per-adapter `StreamDecoder`.
- `packages/ai-gateway/internal/providers/providers_util.go` functions: `defaultTestConnectivity`, any `NormalizeProviderError` (replaced by `ErrorNormalizer` per spec).
- `adapter.go` hook fields (`PrepareRequestFn`, `ParseResponseFn`, `ExtractTextFn`, `BuildStreamRequestFn`, `OpenStreamFn`, `NormalizeErrorFn`, `TestConnectivityFn`) — not re-exported under any name.
- `router/smart_store.go`'s `GetProviderBaseURL` (already deprecated in-thread; formally removed here since callers move to `TargetResolver`).

### 8. Unit tests — `packages/ai-gateway/internal/providers/`

- `types_test.go` — `ProviderError.Error()` formatting; `Format.Valid()` / `Endpoint.Valid()` guards.
- `spec_adapter_test.go` — using a table of fake Transport/SchemaCodec/StreamDecoder/ErrorNormalizer: verify passthrough fast path bypasses codec; verify non-2xx goes through normalizer; verify streaming wraps the decoder session; verify header filtering (no `Authorization` from ingress leaks to upstream).
- `adapter_registry_test.go` — duplicate Format registration is an error; `Get` on unknown Format returns `(nil, false)`; `RegisterBuiltins` registers exactly 9 formats and the registered set equals the `Format` enum.

## Acceptance Criteria

- `go build ./packages/ai-gateway/...` passes after the old `BaseAdapter` is deleted and `specAdapter` + 7 stub `AdapterSpec`s are registered. (Concrete spec bodies are story s2.)
- `go test -race -count=1 ./packages/ai-gateway/internal/providers/...` passes for the scaffolding tests listed in §8.
- No file in `internal/providers/` references `BaseAdapter`, `PrepareRequestFn`, or `defaultTestConnectivity`.
- `providers.Adapter` interface is 3 methods (`Format`, `Execute`, `Probe`) — verified by `grep` in review.
- `provtarget.Resolver` is callable from the `executor`, `router`, and `aiguard` packages without import cycles.

## Out of scope for this story

- Real transport/codec/decoder implementations (s2).
- Any caller rewiring (s3).
- Native ingress routes (s4).
- Hook traffic-adapter wiring (s5).

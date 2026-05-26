package provbuiltins

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubTransport is a minimal Transport implementation used only to make
// AdapterSpec.Valid() report true in the whitebox tests below. None of the
// methods is invoked by registerSpecs.
type stubTransport struct{}

func (stubTransport) BuildURL(_ provcore.CallTarget, _ typology.WireShape, _ bool) (string, error) {
	return "https://stub.invalid", nil
}
func (stubTransport) ApplyAuth(_ *http.Request, _ provcore.CallTarget) error { return nil }
func (stubTransport) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	return nil, nil //nolint:nilnil // intentionally inert; never called by registerSpecs
}
func (stubTransport) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return nil, nil //nolint:nilnil // intentionally inert; never called by registerSpecs
}

// stubStreamDecoder is an inert StreamDecoder. Never invoked by registerSpecs.
type stubStreamDecoder struct{}

func (stubStreamDecoder) Open(_ io.ReadCloser, _ typology.WireShape) (provcore.StreamSession, error) {
	return nil, nil //nolint:nilnil // intentionally inert
}

// stubErrorNormalizer is an inert ErrorNormalizer. Never invoked by registerSpecs.
type stubErrorNormalizer struct{}

func (stubErrorNormalizer) Normalize(_ int, _ http.Header, _ []byte) *provcore.ProviderError {
	return nil
}

// TestRegister_NilLogger_DefaultsToSlogDefault exercises the `if log == nil`
// branch of Register; passing nil must fall back to slog.Default() rather
// than panic.
func TestRegister_NilLogger_DefaultsToSlogDefault(t *testing.T) {
	reg := provcore.NewRegistry()
	// nil logger and nil allowlist are both legal per the doc comment.
	Register(reg, nil, nil)
	reg.Freeze()
	if len(reg.List()) != len(provcore.AllFormats()) {
		t.Fatalf("nil-logger Register seeded %d formats, want %d",
			len(reg.List()), len(provcore.AllFormats()))
	}
}

// TestSchemaCodecs_CoversEveryFormat asserts SchemaCodecs returns a non-nil
// SchemaCodec for every Format declared in provcore.AllFormats().
func TestSchemaCodecs_CoversEveryFormat(t *testing.T) {
	codecs := SchemaCodecs(slog.Default())
	for _, f := range provcore.AllFormats() {
		c, ok := codecs[f]
		if !ok {
			t.Errorf("SchemaCodecs missing format %q", f)
			continue
		}
		if c == nil {
			t.Errorf("SchemaCodecs[%q] is nil", f)
		}
	}
	// Verify symmetry: no extra formats either.
	if len(codecs) != len(provcore.AllFormats()) {
		t.Errorf("SchemaCodecs returned %d entries, AllFormats has %d",
			len(codecs), len(provcore.AllFormats()))
	}
}

// TestSchemaCodecs_NilLogger_DefaultsToSlogDefault covers the nil-logger
// branch of SchemaCodecs.
func TestSchemaCodecs_NilLogger_DefaultsToSlogDefault(t *testing.T) {
	codecs := SchemaCodecs(nil)
	if codecs[provcore.FormatOpenAI] == nil {
		t.Fatal("SchemaCodecs(nil) lost OpenAI codec")
	}
}

// validStub returns an AdapterSpec whose Valid() reports true. Used by the
// whitebox panic-path tests below.
func validStub(format provcore.Format) provcore.AdapterSpec {
	openaiCodecs := SchemaCodecs(slog.Default())
	openai := openaiCodecs[provcore.FormatOpenAI]
	// Build a spec that satisfies AdapterSpec.Valid() (Format valid +
	// non-nil Transport / SchemaCodec / StreamDecoder / ErrorNormalizer)
	// by borrowing OpenAI's components but stamping a different Format
	// so registerSpecs can distinguish entries when we want duplicates.
	return provcore.AdapterSpec{
		Format:          format,
		Transport:       stubTransport{},
		SchemaCodec:     openai,
		StreamDecoder:   stubStreamDecoder{},
		ErrorNormalizer: stubErrorNormalizer{},
	}
}

// TestRegisterSpecs_InvalidSpecPanics covers the `if !s.Valid()` panic
// branch: an AdapterSpec whose Format is empty (Valid() returns false)
// must trigger the panic with the expected message.
func TestRegisterSpecs_InvalidSpecPanics(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("registerSpecs with invalid spec must panic")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "invalid spec") {
			t.Fatalf("panic msg = %q, want substring 'invalid spec'", msg)
		}
	}()
	reg := provcore.NewRegistry()
	bad := provcore.AdapterSpec{Format: ""} // Valid() == false
	registerSpecs(reg, []provcore.AdapterSpec{bad}, nil, slog.Default(), nil)
}

// TestRegisterSpecs_DuplicateFormatPanics covers the `if seen[s.Format]`
// panic branch: passing the same format twice must trigger the duplicate
// panic.
func TestRegisterSpecs_DuplicateFormatPanics(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("registerSpecs with duplicate format must panic")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "duplicate spec") {
			t.Fatalf("panic msg = %q, want substring 'duplicate spec'", msg)
		}
	}()
	reg := provcore.NewRegistry()
	specs := []provcore.AdapterSpec{
		validStub(provcore.FormatOpenAI),
		validStub(provcore.FormatOpenAI), // dup
	}
	registerSpecs(reg, specs, nil, slog.Default(), nil)
}

// TestRegisterSpecs_MissingFormatPanics covers the trailing
// `if !seen[want]` panic: the wantFormats slice asks for a format that
// the specs slice does not provide.
func TestRegisterSpecs_MissingFormatPanics(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("registerSpecs with missing required format must panic")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "missing format") {
			t.Fatalf("panic msg = %q, want substring 'missing format'", msg)
		}
	}()
	reg := provcore.NewRegistry()
	specs := []provcore.AdapterSpec{validStub(provcore.FormatOpenAI)}
	want := []provcore.Format{provcore.FormatOpenAI, provcore.FormatAnthropic}
	registerSpecs(reg, specs, nil, slog.Default(), want)
}

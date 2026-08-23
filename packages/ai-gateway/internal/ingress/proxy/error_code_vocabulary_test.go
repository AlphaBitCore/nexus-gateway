package proxy

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// upperSnake is the caller-facing contract for error.code on this surface: an
// UPPER_SNAKE machine code, or absent. The provider vocabulary is lower_snake
// and lives on the other side of the normalisation boundary; a value crossing
// from there to a caller is a namespace leak, not a synonym.
var upperSnake = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// TestUnnamedGatewayErrorDoesNotBorrowTheProviderVocabulary pins the default
// that leaked in production.
//
// Measured on prod: an unrecognised content part came back as
//
//	{"error":{"code":"invalid_request","message":"nexus: this provider does not
//	 accept a nexus_probe_part content part","type":"invalid_request_error"}}
//
// one second before a neighbouring refusal answered REDACT_INFLIGHT_UNSUPPORTED.
// One surface, two vocabularies. writeCodecErr defaulted an unnamed typed error
// to provcore.CodeInvalidRequest — the PROVIDER constant, the same family as
// client_gone — so every typed gateway error that does not name itself handed
// the caller a code from the wrong namespace, and stamped it on the row too.
func TestCodecRefusalReachesTheCallerInTheGatewayVocabulary(t *testing.T) {
	h := &Handler{deps: &Deps{}}

	// The FIRST version of this test drove writeCodecErr with an empty Code and
	// asserted the default. It went red, the default was changed, it went green
	// — and production kept answering "invalid_request", because no producer on
	// this path leaves Code empty. specutil.errContentRefused sets
	// Code: provcore.CodeInvalidRequest explicitly, and 82 codec sites do the
	// same. A gate red on an input nothing produces proves nothing about the
	// defect it was written for.
	//
	// So this drives what the producers actually raise.
	for _, tc := range []struct {
		name string
		in   *provcore.ProviderError
		want string
	}{
		{"a content-part refusal, the exact prod case",
			&provcore.ProviderError{Status: http.StatusBadRequest, Code: provcore.CodeInvalidRequest,
				Type: "nexus_field_unsupported", Message: "nexus: this provider does not accept a made_up content part"},
			"INVALID_REQUEST"},
		{"an endpoint the adapter does not serve",
			&provcore.ProviderError{Status: http.StatusBadRequest, Code: provcore.CodeEndpointUnsupported,
				Message: "nexus: this provider has no rerank leg"},
			"ENDPOINT_NOT_SUPPORTED"},
		{"a code the gateway already owns passes through untouched",
			&provcore.ProviderError{Status: http.StatusBadRequest, Code: "SPEND_LIMIT_EXCEEDED",
				Message: "too many"},
			"SPEND_LIMIT_EXCEEDED"},
		{"no code at all still names itself",
			&provcore.ProviderError{Status: http.StatusBadRequest, Message: "refused"},
			"INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &audit.Record{IngressFormat: string(provcore.FormatOpenAI)}
			w := &testResponseWriter{}
			h.writeCodecErr(w, rec, tc.in, "canonicalize ingress body: ")

			got := gjson.GetBytes(w.body, "error.code").String()
			if got != tc.want {
				t.Errorf("error.code = %q, want %q — the caller-facing vocabulary is UPPER_SNAKE; "+
					"%q belongs to the provider namespace on the far side of normalisation. Body: %s",
					got, tc.want, tc.in.Code, w.body)
			}
			if rec.ErrorCode != tc.want {
				t.Errorf("row error_code = %q, want %q — row and wire must agree", rec.ErrorCode, tc.want)
			}
		})
	}

	// An untyped error keeps the fallback path's own named code.
	rec := &audit.Record{IngressFormat: string(provcore.FormatOpenAI)}
	w := &testResponseWriter{}
	h.writeCodecErr(w, rec, errors.New("boom"), "prefix: ")
	if got := gjson.GetBytes(w.body, "error.code").String(); !upperSnake.MatchString(got) {
		t.Errorf("untyped fallback code = %q, want UPPER_SNAKE (%s)", got, w.body)
	}
}

// TestEveryCanonicalCodeHasAGatewaySpelling makes the translation exhaustive.
// CanonicalCodes exists so a new code cannot quietly skip the surfaces that must
// know about it; this adds the caller-facing surface to that list. Without it a
// code added tomorrow reaches a caller in the provider's lower_snake spelling
// and nothing notices — which is how 82 sites got there.
func TestEveryCanonicalCodeHasAGatewaySpelling(t *testing.T) {
	for _, code := range provcore.CanonicalCodes {
		got := gatewayErrorCode(code)
		if !upperSnake.MatchString(got) {
			t.Errorf("canonical %q maps to %q, which is not UPPER_SNAKE — add it to gatewayErrorCode",
				code, got)
		}
	}
	if got := gatewayErrorCode(""); got != "INVALID_REQUEST" {
		t.Errorf("the empty code maps to %q, want INVALID_REQUEST", got)
	}
}

// TestEveryLiteralErrorCodeIsUpperSnake sweeps the vocabulary itself, so a code
// added tomorrow cannot arrive in the provider's spelling. Structural walk
// rather than a grep: the property is "this literal sits in the code argument of
// an error writer", which source text alone cannot answer.
func TestEveryLiteralErrorCodeIsUpperSnake(t *testing.T) {
	// (function name) -> which positional argument carries the code.
	codeArg := map[string]int{
		"writeError": 3, "writeDetailedErr": 3, "writeIngressError": 3,
		"writeEstimateError": 2, "rejectWithBespokeEnvelope": 3,
	}

	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	var bad []string
	inspected := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			default:
				return true
			}
			idx, ok := codeArg[name]
			if !ok || idx >= len(call.Args) {
				return true
			}
			lit, ok := call.Args[idx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true // a variable or constant; the runtime test above covers those
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || v == "" {
				return true
			}
			inspected++
			if !upperSnake.MatchString(v) {
				bad = append(bad, path+" "+name+"(… "+lit.Value+" …)")
			}
			return true
		})
	}
	if len(bad) > 0 {
		t.Fatalf("these error codes reach a caller in the provider's lower_snake spelling:\n  %s",
			strings.Join(bad, "\n  "))
	}
	// A sweep that inspected nothing is a sweep that passes forever. Renaming
	// any writer would make codeArg stale and leave this silently green, which
	// is the failure mode its sibling guard in error_writer_ast_test.go already
	// protects against.
	if inspected == 0 {
		t.Fatal("no error-code literal was inspected; the codeArg map has gone stale against " +
			"the writers' real names or signatures, so this guard binds nothing")
	}
	t.Logf("inspected %d error-code literals across the package", inspected)
}

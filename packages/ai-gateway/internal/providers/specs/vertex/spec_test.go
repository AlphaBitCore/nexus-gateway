package vertex_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/vertex"
)

func TestVertex_BuildURL(t *testing.T) {
	tr := vertex.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "gemini-1.5-pro",
			Extras: map[string]string{
				"gcp.projectId": "proj",
				"gcp.location":  "us-central1",
			},
		},
		typology.WireShapeVertexGenerateContent,
		false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, "aiplatform.googleapis.com") {
		t.Errorf("host: %s", got)
	}
	if !strings.Contains(got, "/v1/projects/proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent") {
		t.Errorf("path: %s", got)
	}
}

func TestVertex_BuildURL_Stream(t *testing.T) {
	tr := vertex.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "gemini-1.5-pro",
			Extras: map[string]string{
				"gcp.projectId": "proj",
			},
		},
		typology.WireShapeVertexGenerateContent,
		true,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, ":streamGenerateContent?alt=sse") {
		t.Errorf("stream URL malformed: %s", got)
	}
}

func TestVertex_ApplyAuth_UsesBearerToken(t *testing.T) {
	tr := vertex.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{
		Extras: map[string]string{"gcp.bearerToken": "ya29.x"},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("Authorization") != "Bearer ya29.x" {
		t.Errorf("Authorization: %q", req.Header.Get("Authorization"))
	}
}

func TestVertex_ApplyAuth_MissingAll(t *testing.T) {
	tr := vertex.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{})
	if err == nil {
		t.Fatalf("expected error when no credential source is provided")
	}
}

func TestVertex_Execute_Stream_WiresGeminiDecoder(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
		``,
	}, "\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, raw)
	}))
	defer server.Close()

	a := provdispatch.NewSpecAdapter(vertex.NewSpec(slog.Default()), slog.Default())
	resp, err := a.Execute(context.Background(), provcore.Request{
		WireShape:   typology.WireShapeVertexGenerateContent,
		BodyFormat: provcore.FormatVertex,
		Body:       []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		Stream:     true,
		Target: provcore.CallTarget{
			BaseURL:         server.URL,
			ProviderModelID: "gemini-1.5-pro",
			Extras: map[string]string{
				"gcp.projectId":   "p",
				"gcp.bearerToken": "t",
			},
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer resp.Stream.Close() //nolint:errcheck

	var text string
	done := false
	for {
		ch, err := resp.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		text += ch.Delta
		if ch.Done {
			done = true
		}
	}
	if text != "hi!" {
		t.Errorf("stream text=%q", text)
	}
	if !done {
		t.Errorf("expected done chunk")
	}
}

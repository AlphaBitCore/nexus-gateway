//go:build realtimeprobe

// realtime-probe — per-model probe of the gateway's /v1/realtime WebSocket.
//
//	Run with: cd tests/scenarios && \
//	  NEXUS_GW_WS=wss://api.<domain> NEXUS_VK=$(cat /tmp/nexus-smoke-vk.txt) \
//	  go run -tags realtimeprobe ../scripts/realtime-probe.go [model ...]
//
// For conversational models the proof is business evidence: the model is
// instructed to say a nonce word and the response transcript must carry it.
// For transcription-oriented realtime models (translate / whisper) the probe
// stops at the session handshake and SAYS SO — a handshake is proof the
// gateway relays the socket and the model accepts a session, not proof of
// transcription quality; over-claiming here is how coverage reports rot.
//
// Build-tagged so it doesn't fight the scenarios package's main_test.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const nonce = "realtimeoak"

func probe(gwWS, vk, model string, conversational bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	url := gwWS + "/v1/realtime?model=" + model
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+vk)
	hdr.Set("OpenAI-Beta", "realtime=v1")
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		detail := ""
		if resp != nil && resp.Body != nil {
			buf := make([]byte, 512)
			n, _ := resp.Body.Read(buf)
			detail = " body=" + string(buf[:n])
		}
		return "DIAL_FAILED: " + err.Error() + detail
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(1 << 22)

	readEvent := func() (string, map[string]any, error) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return "", nil, err
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			return "", nil, err
		}
		t, _ := ev["type"].(string)
		return t, ev, nil
	}

	// Every session opens with session.created; nothing else counts as a
	// live relay.
	t, first, err := readEvent()
	if err != nil {
		return "NO_FIRST_EVENT: " + err.Error()
	}
	if t != "session.created" {
		b, _ := json.Marshal(first)
		return "FIRST_EVENT_NOT_SESSION_CREATED: " + t + " " + string(b)
	}
	if !conversational {
		// A transcription model is not a session model (the upstream error
		// for using one as such says to pass it as
		// audio.input.transcription.model). The caller dials the session on a
		// conversational model and this branch proves the transcription model
		// is ACCEPTED as the session's input transcriber.
		if tm := os.Getenv("NEXUS_RT_TRANSCRIBER"); tm != "" {
			upd := map[string]any{
				"type": "session.update",
				"session": map[string]any{
					"type": "realtime",
					"audio": map[string]any{
						"input": map[string]any{
							"transcription": map[string]any{"model": tm},
						},
					},
				},
			}
			buf, _ := json.Marshal(upd)
			if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
				return "WRITE_FAILED: " + err.Error()
			}
			t, ev, err := readEvent()
			if err != nil {
				return "NO_UPDATE_ACK: " + err.Error()
			}
			if t == "session.updated" {
				return "TRANSCRIBER_ACCEPTED: session.updated with transcription.model=" + tm
			}
			b, _ := json.Marshal(ev)
			return "TRANSCRIBER_REJECTED: " + string(b)
		}
		return "HANDSHAKE_OK (transcription-model: session relayed; content quality not probed here)"
	}

	req := map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"output_modalities": []string{"text"},
			"instructions":      "Reply with exactly the single word: " + nonce,
		},
	}
	buf, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		return "WRITE_FAILED: " + err.Error()
	}

	var transcript strings.Builder
	for {
		t, ev, err := readEvent()
		if err != nil {
			return "STREAM_ENDED_EARLY: " + err.Error() + " transcript=" + transcript.String()
		}
		switch t {
		case "response.text.delta", "response.output_text.delta", "response.audio_transcript.delta", "response.output_audio_transcript.delta":
			if d, ok := ev["delta"].(string); ok {
				transcript.WriteString(d)
			}
		case "response.done":
			got := transcript.String()
			if strings.Contains(strings.ToLower(got), nonce) {
				return "SPOKE_THE_NONCE: " + strings.TrimSpace(got)
			}
			return "RESPONDED_WITHOUT_NONCE: " + strings.TrimSpace(got)
		case "error":
			b, _ := json.Marshal(ev)
			return "SERVER_ERROR: " + string(b)
		}
	}
}

func main() {
	gwWS := os.Getenv("NEXUS_GW_WS")
	vk := os.Getenv("NEXUS_VK")
	if gwWS == "" || vk == "" {
		fmt.Println("NEXUS_GW_WS and NEXUS_VK are required")
		os.Exit(1)
	}
	models := os.Args[1:]
	if len(models) == 0 {
		models = []string{"gpt-realtime-2.1", "gpt-realtime-2.1-mini",
			"gpt-realtime-translate", "gpt-realtime-whisper"}
	}
	for _, m := range models {
		if strings.Contains(m, "translate") || strings.Contains(m, "whisper") {
			// Not a session model (the upstream error for using one as such
			// says to pass it as audio.input.transcription.model). The
			// session dials on a conversational model; the model under test
			// is the transcriber the session is asked to accept.
			os.Setenv("NEXUS_RT_TRANSCRIBER", m)
			fmt.Printf("%s: %s\n", m, probe(gwWS, vk, "gpt-realtime-2.1", false))
			os.Unsetenv("NEXUS_RT_TRANSCRIBER")
			continue
		}
		fmt.Printf("%s: %s\n", m, probe(gwWS, vk, m, true))
	}
}

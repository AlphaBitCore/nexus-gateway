package cursor

import (
	"bytes"
	"github.com/goccy/go-json"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// isAgentRunPath reports the new Cursor agent service
// (/agent.v1.AgentService/Run + siblings). Both the Cursor app and the
// cursor-agent CLI now ride this service; its connect-RPC frames carry the
// conversation as OpenAI-compat {"role","content":[{type,text}]} JSON (and
// Lexical {"root":...} blocks for the user's typed message) embedded in
// protobuf string fields — a different shape from the legacy
// /aiserver.v1.AiService/Stream* GetChatRequest. Keying on the embedded JSON
// (a stable OpenAI-compat interface) is resilient to Cursor reshuffling the
// surrounding protobuf field numbers.
func isAgentRunPath(path string) bool {
	return strings.HasPrefix(path, "/agent.v1.AgentService/")
}

// agentTurn is one conversation turn recovered from an agent-service frame.
// text already includes any rendered tool-call lines; tools holds the raw
// tool-call JSON objects (for the streaming ToolCallSegments channel).
type agentTurn struct {
	role  string
	text  string
	tools []string
}

// agentRunConversation is the decoded conversation of a Cursor
// /agent.v1.AgentService/Run connect-RPC body (request or response).
type agentRunConversation struct {
	Roles    []string
	Contents []string
	Tools    []string
	Model    string
}

// decodeAgentRunBody unwraps the connect-RPC frames of a Cursor agent-service
// body, gzip-decompressing each compressed frame, and reconstructs the
// conversation from the embedded OpenAI-compat / Lexical JSON. The agent
// response restreams a growing snapshot of the whole conversation every frame,
// so turns are de-duplicated by (role,text) and tool-call JSON by exact bytes.
// A bare (non-framed) body is walked directly as a fallback. ok is false when
// no conversation text was recovered, so the caller can fall through to the
// generic Connect-RPC detector.
func decodeAgentRunBody(raw []byte) (agentRunConversation, bool) {
	var conv agentRunConversation
	seenTurn := map[string]bool{}
	seenTool := map[string]bool{}
	add := func(t agentTurn) {
		if t.text != "" {
			key := t.role + "\x00" + t.text
			if !seenTurn[key] {
				seenTurn[key] = true
				conv.Roles = append(conv.Roles, t.role)
				conv.Contents = append(conv.Contents, t.text)
			}
		}
		for _, tc := range t.tools {
			if !seenTool[tc] {
				seenTool[tc] = true
				conv.Tools = append(conv.Tools, tc)
			}
		}
	}
	process := func(payload []byte) {
		turns, model := extractAgentTurns(payload)
		if model != "" && conv.Model == "" {
			conv.Model = model
		}
		for _, t := range turns {
			add(t)
		}
	}

	r := bytes.NewReader(raw)
	frames := 0
	for {
		flags, payload, err := streaming.ReadConnectRPCFrame(r)
		if err != nil {
			break
		}
		if len(payload) > 0 {
			process(streaming.MaybeGunzipConnectFrame(flags, payload))
			frames++
		}
		if flags&streaming.ConnectFlagEndStream != 0 {
			break
		}
	}
	if frames == 0 {
		// Body was not connect-framed (or a bare prefix capture) — walk it raw.
		process(raw)
	}
	return conv, len(conv.Contents) > 0 || len(conv.Tools) > 0
}

// extractAgentTurns walks one decoded frame payload for embedded conversation
// JSON, returning its turns and any model name found in providerOptions.
// {"role",...} objects are OpenAI-compat messages; {"root",...} objects are
// Lexical editor blocks holding the user's typed message.
func extractAgentTurns(frame []byte) (turns []agentTurn, model string) {
	walkCursorAgentStrings(frame, 0, func(s string) {
		t := strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(t, `{"role"`):
			role, text, tools, m := parseCursorRoleMessage(t)
			if m != "" && model == "" {
				model = m
			}
			if text != "" || len(tools) > 0 {
				turns = append(turns, agentTurn{role: role, text: text, tools: tools})
			}
		case strings.HasPrefix(t, `{"root"`):
			if txt := parseLexicalText(t); txt != "" {
				turns = append(turns, agentTurn{role: "user", text: txt})
			}
		}
	})
	return turns, model
}

// extractCursorAgentFrame recovers conversation text from one Cursor
// agent-service connect-RPC frame for the streaming-response path
// (ExtractStreamChunk). Each turn's text becomes a "[role] text" segment; raw
// tool-call JSON rides the separate ToolCallSegments channel. Returns empty
// content for frames with no conversation JSON (most frames are
// transport/metadata).
func extractCursorAgentFrame(frame []byte) traffic.NormalizedContent {
	turns, _ := extractAgentTurns(frame)
	var segs, tools []string
	for _, t := range turns {
		if t.text != "" {
			segs = append(segs, "["+t.role+"] "+t.text)
		}
		tools = append(tools, t.tools...)
	}
	return traffic.NormalizedContent{Segments: segs, ToolCallSegments: tools}
}

// walkCursorAgentStrings recursively walks a protobuf message, invoking emit for
// every length-delimited field that decodes to a JSON object ({"…). Sub-messages
// (non-printable length-delimited fields) are descended into; leaf strings that
// are not JSON objects (UUIDs, paths) are ignored. Depth is bounded so a hostile
// or malformed frame cannot drive unbounded recursion.
func walkCursorAgentStrings(b []byte, depth int, emit func(string)) {
	if depth > 8 {
		return
	}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num == 0 {
			return
		}
		b = b[n:]
		if typ == protowire.BytesType {
			s, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return
			}
			// A length-delimited field is either an embedded JSON object (emit
			// it) or a nested sub-message (descend). Descending into a leaf
			// string (UUID / path / blob) is harmless — protowire bails out fast
			// when the bytes are not valid protobuf — so we don't need a fragile
			// "is this text?" heuristic that risks hiding JSON nested under a
			// mostly-printable sub-message.
			if isJSONObjectStart(s) {
				emit(string(s))
			} else {
				walkCursorAgentStrings(s, depth+1, emit)
			}
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return
		}
		b = b[m:]
	}
}

// isJSONObjectStart reports whether s, after leading whitespace, begins with `{"`.
func isJSONObjectStart(s []byte) bool {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return i+1 < len(s) && s[i] == '{' && s[i+1] == '"'
}

// parseCursorRoleMessage decodes an OpenAI-compat {"role","content"} message,
// returning the role, the concatenated visible text (assistant prose, tool
// results, and readable "→ tool: arg" lines for tool calls), the raw tool-call
// JSON objects, and the model name when present in a part's providerOptions.
// Returns ("", "", nil, "") on a parse failure or a message with no role.
// "redacted-reasoning" parts are Cursor's server-side-encrypted reasoning and
// carry no readable text.
func parseCursorRoleMessage(jsonStr string) (role, text string, tools []string, model string) {
	var m struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal([]byte(jsonStr), &m) != nil || m.Role == "" {
		return "", "", nil, ""
	}
	var sb strings.Builder
	writeLine := func(s string) {
		if s == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(s)
	}
	for _, raw := range m.Content {
		var part struct {
			Type            string          `json:"type"`
			Text            string          `json:"text"`
			Result          json.RawMessage `json:"result"`
			ProviderOptions struct {
				Cursor struct {
					ModelName string `json:"modelName"`
				} `json:"cursor"`
			} `json:"providerOptions"`
		}
		if json.Unmarshal(raw, &part) != nil {
			continue
		}
		if part.ProviderOptions.Cursor.ModelName != "" && model == "" {
			model = part.ProviderOptions.Cursor.ModelName
		}
		switch part.Type {
		case "text", "output_text", "":
			writeLine(part.Text)
		case "tool-result", "tool_result":
			writeLine(toolResultText(part.Result))
		case "tool-call", "tool_call", "tool_use", "function_call":
			tools = append(tools, string(raw))
			writeLine(renderToolCall(raw))
			// redacted-reasoning / image / other types carry no readable text.
		}
	}
	return m.Role, strings.TrimSpace(sb.String()), tools, model
}

// toolResultText extracts the textual output of a tool-result part. The result
// is usually a JSON string (shell output, file contents); when it is a JSON
// object/array the compact encoding is used so the value is never dropped.
func toolResultText(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(result, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(result))
}

// renderToolCall turns a tool-call content part into a readable one-line
// summary ("→ Shell: echo hi") so an agent transcript reads as a conversation
// rather than opaque JSON. The raw object is still preserved separately for the
// ToolCallSegments channel.
func renderToolCall(raw json.RawMessage) string {
	var tc struct {
		ToolName string          `json:"toolName"`
		Name     string          `json:"name"`
		Args     json.RawMessage `json:"args"`
		Input    json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return ""
	}
	name := tc.ToolName
	if name == "" {
		name = tc.Name
	}
	if name == "" {
		name = "tool"
	}
	arg := compactToolArg(tc.Args)
	if arg == "" {
		arg = compactToolArg(tc.Input)
	}
	if arg == "" {
		return "→ " + name
	}
	return "→ " + name + ": " + arg
}

// compactToolArg pulls the most descriptive scalar out of a tool's argument
// object (command / path / query / pattern / input), falling back to a
// length-bounded compaction of the whole object so a long argument blob can't
// bloat the transcript.
func compactToolArg(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) == nil {
		for _, key := range []string{"command", "path", "query", "pattern", "input", "file_path", "filePath"} {
			if v, ok := obj[key]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
					return truncateArg(strings.TrimSpace(s))
				}
			}
		}
	}
	return truncateArg(string(args))
}

// truncateArg bounds a rendered argument to keep transcript lines readable.
func truncateArg(s string) string {
	const max = 160
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// parseLexicalText extracts the concatenated text of a Lexical editor document
// (the user's typed message), recursively collecting every node's text field.
func parseLexicalText(jsonStr string) string {
	var root any
	if json.Unmarshal([]byte(jsonStr), &root) != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(n any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			if x["type"] == "text" {
				if t, ok := x["text"].(string); ok {
					sb.WriteString(t)
				}
			}
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(root)
	return strings.TrimSpace(sb.String())
}

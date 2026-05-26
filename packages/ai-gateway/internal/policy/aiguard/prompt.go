package aiguard

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// RenderInput carries the placeholders available to the judge prompt
// template. Matches spec §4.6 exactly.
type RenderInput struct {
	DetectorType   string
	Content        string
	UpstreamTags   []string
	TargetProvider string
	TargetModel    string
}

// TagsJoined formats UpstreamTags into a compact comma-separated string
// for the judge. Empty → "(none)".
func (r RenderInput) TagsJoined() string {
	if len(r.UpstreamTags) == 0 {
		return "(none)"
	}
	return strings.Join(r.UpstreamTags, ", ")
}

// Render executes tmpl with in as the data context. Returns the rendered
// string or a parse/execute error. Placeholders use text/template syntax:
// {{.DetectorType}}, {{.Content}}, {{.TagsJoined}}, etc.
func Render(tmpl string, in RenderInput) (string, error) {
	t, err := template.New("aiguard-prompt").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("aiguard: prompt template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, in); err != nil {
		return "", fmt.Errorf("aiguard: prompt template execute: %w", err)
	}
	return buf.String(), nil
}

// DefaultPrompt is the canonical judge prompt shipped with Nexus.
// Customer editable via the Admin UI.
const DefaultPrompt = `You are a security classifier for enterprise AI traffic. Analyze the
provided CONTENT for the detector type {{.DetectorType}}. Return ONLY
valid JSON matching this schema:

{"decision":"approve|reject_hard|block_soft|modify",
 "confidence":<0.0-1.0>,
 "reason":"<short human-readable explanation>",
 "labels":["<tag>","<tag>"],
 "redactions":[
   {"start":<int>,"end":<int>,"replacement":"<text>","action":"redact|strip|replace","reason":"<why>"}
 ]}

Guidelines:
- reject_hard: clear, high-confidence policy violation. Use sparingly.
- block_soft: likely violation; the caller may warn instead of blocking.
- approve: content is acceptable for the detector type.
- modify: emit one ` + "`redactions[]`" + ` entry per sensitive span. ` + "`start`" + `/` + "`end`" + `
  are UTF-8 byte offsets into the verbatim CONTENT string between <<<…>>>;
  ` + "`end`" + ` is exclusive. ` + "`replacement`" + ` is the placeholder to substitute
  (e.g. "[REDACTED_EMAIL]"). ` + "`action`" + ` defaults to "redact" when omitted.

Always emit ` + "`redactions`" + ` as an array (possibly empty). Never return the
whole sanitised body — only the spans.

Example for an email leak (CONTENT = "contact me at jane@example.com please"):
{"decision":"modify","confidence":0.95,"reason":"email PII",
 "labels":["pii:email"],
 "redactions":[{"start":14,"end":31,"replacement":"[REDACTED_EMAIL]","action":"redact"}]}

Context:
- Target provider: {{.TargetProvider}}
- Target model: {{.TargetModel}}
- Upstream tags: {{.TagsJoined}}

CONTENT:
<<<
{{.Content}}
>>>
`

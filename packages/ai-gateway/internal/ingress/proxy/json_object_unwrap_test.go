package proxy

import (
	"strconv"
	"testing"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
)

// chatBody builds a minimal canonical chat.completion with one choice whose
// message content is exactly s (JSON-escaped by the marshaller).
func chatBody(s string) []byte {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type choice struct {
		Index   int `json:"index"`
		Message msg `json:"message"`
	}
	type body struct {
		Object  string   `json:"object"`
		Choices []choice `json:"choices"`
	}
	raw, _ := json.Marshal(body{
		Object:  "chat.completion",
		Choices: []choice{{Message: msg{Role: "assistant", Content: s}}},
	})
	return raw
}

func jsonObjectMeta() any {
	return map[string]any{"chat": map[string]any{"json_object_requested": true}}
}

func TestUnwrapJSONObjectFences_StripsFencedJSON(t *testing.T) {
	// The exact shape claude-haiku-4-5 returned on staging for a json_object
	// request — json.loads on this fails at char 0.
	body := chatBody("```json\n{\n  \"name\": \"Margaret Chen\",\n  \"age\": 34\n}\n```")

	got := unwrapJSONObjectFences(body, jsonObjectMeta())

	content := gjson.GetBytes(got, "choices.0.message.content").Str
	if !gjson.Valid(content) {
		t.Fatalf("content still does not parse as JSON: %q", content)
	}
	if name := gjson.Get(content, "name").Str; name != "Margaret Chen" {
		t.Errorf("unwrapped content lost data: name=%q in %q", name, content)
	}
	if role := gjson.GetBytes(got, "choices.0.message.role").Str; role != "assistant" {
		t.Errorf("rewrite clobbered the role: %q", role)
	}
}

func TestUnwrapJSONObjectFences_StripsFenceWithoutLanguageTag(t *testing.T) {
	body := chatBody("```\n{\"a\":1}\n```")

	got := unwrapJSONObjectFences(body, jsonObjectMeta())

	if content := gjson.GetBytes(got, "choices.0.message.content").Str; content != `{"a":1}` {
		t.Errorf("content = %q, want %q", content, `{"a":1}`)
	}
}

func TestUnwrapJSONObjectFences_UnwrapsEveryChoice(t *testing.T) {
	body := []byte(`{"choices":[
		{"index":0,"message":{"role":"assistant","content":"` + "```json\\n{\\\"a\\\":1}\\n```" + `"}},
		{"index":1,"message":{"role":"assistant","content":"` + "```json\\n{\\\"b\\\":2}\\n```" + `"}}
	]}`)

	got := unwrapJSONObjectFences(body, jsonObjectMeta())

	for i, want := range []string{`{"a":1}`, `{"b":2}`} {
		path := "choices." + strconv.Itoa(i) + ".message.content"
		if c := gjson.GetBytes(got, path).Str; c != want {
			t.Errorf("choice %d content = %q, want %q", i, c, want)
		}
	}
}

func TestUnwrapJSONObjectFences_LeavesNonJSONFenceAlone(t *testing.T) {
	// A fenced Python snippet is not JSON. Stripping it would silently mangle a
	// legitimate answer, so the parse check must reject it.
	original := "```python\nprint('hi')\n```"
	body := chatBody(original)

	got := unwrapJSONObjectFences(body, jsonObjectMeta())

	if c := gjson.GetBytes(got, "choices.0.message.content").Str; c != original {
		t.Errorf("non-JSON fence was rewritten to %q", c)
	}
}

func TestUnwrapJSONObjectFences_NoOpCases(t *testing.T) {
	fenced := chatBody("```json\n{\"a\":1}\n```")

	cases := []struct {
		name string
		body []byte
		meta any
	}{
		{"json_object not requested", fenced, map[string]any{}},
		{"nil metadata", fenced, nil},
		{"bare JSON already", chatBody(`{"a":1}`), jsonObjectMeta()},
		{"prose answer", chatBody("here you go"), jsonObjectMeta()},
		{"no choices array", []byte(`{"error":{"message":"x"}}`), jsonObjectMeta()},
		{"empty body", nil, jsonObjectMeta()},
		// A tool-call turn has content:null; a multimodal turn has an array. Neither
		// is a string, so there is nothing to unwrap.
		{"null content", []byte(`{"choices":[{"message":{"content":null}}]}`), jsonObjectMeta()},
		{"array content", []byte(`{"choices":[{"message":{"content":[{"type":"text","text":"x"}]}}]}`), jsonObjectMeta()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapJSONObjectFences(tc.body, tc.meta); string(got) != string(tc.body) {
				t.Errorf("body rewritten but should have been left alone:\n got %s\nwant %s", got, tc.body)
			}
		})
	}
}

func TestPreStampJSONObjectMeta(t *testing.T) {
	t.Run("stamps when json_object requested", func(t *testing.T) {
		md := preStampJSONObjectMeta(nil, []byte(`{"response_format":{"type":"json_object"}}`))
		if !jsonObjectRequested(md) {
			t.Errorf("json_object request was not stamped: %#v", md)
		}
	})
	t.Run("leaves metadata untouched otherwise", func(t *testing.T) {
		for _, reqBody := range []string{
			`{"response_format":{"type":"json_schema"}}`,
			`{"model":"gpt-4o"}`,
			`{}`,
		} {
			md := preStampJSONObjectMeta(nil, []byte(reqBody))
			if jsonObjectRequested(md) {
				t.Errorf("%s should not stamp json_object", reqBody)
			}
		}
	})
	t.Run("preserves sibling metadata keys", func(t *testing.T) {
		existing := map[string]any{"embedding": map[string]any{"batch_size": 3}}
		md := preStampJSONObjectMeta(existing, []byte(`{"response_format":{"type":"json_object"}}`))
		m, ok := md.(map[string]any)
		if !ok {
			t.Fatalf("metadata is not a map: %T", md)
		}
		if _, still := m["embedding"]; !still {
			t.Errorf("stamping dropped the pre-existing embedding submap: %#v", m)
		}
	})
}

func TestStripJSONCodeFence(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"json tag", "```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"uppercase tag", "```JSON\n{\"a\":1}\n```", `{"a":1}`, false},
		{"no tag", "```\n{\"a\":1}\n```", `{"a":1}`, false},
		{"array payload", "```json\n[1,2]\n```", `[1,2]`, false},
		{"surrounding whitespace", "  ```json\n{\"a\":1}\n```  ", `{"a":1}`, false},
		{"unfenced", `{"a":1}`, "", true},
		// An over-long first line is not a language tag, so it must be treated as
		// part of the payload — which then fails the JSON check.
		{"long first line", "```averyverylongfencetagindeed\n{\"a\":1}\n```", "", true},
		{"punctuated first line", "```not-a-tag\n{\"a\":1}\n```", "", true},
		{"open fence only", "```json\n{\"a\":1}", "", true},
		{"non-JSON body", "```python\nprint(1)\n```", "", true},
		{"empty fence", "```\n\n```", "", true},
		{"prose then fence", "sure:\n```json\n{\"a\":1}\n```", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stripJSONCodeFence(tc.in)
			if tc.wantErr {
				if ok {
					t.Errorf("expected no unwrap, got %q", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected an unwrap for %q", tc.in)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

package iam

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
)

// StringList is a JSON-shape-tolerant []string used by AWS-style IAM
// policy documents. AWS accepts either a single string or an array of
// strings for both Action and Resource — for example all of the
// following are valid Statement fragments:
//
//	"Action": "s3:PutObject"            // single string
//	"Action": ["s3:GetObject", "s3:..."] // array
//	"Resource": "*"                      // single string
//	"Resource": ["arn:aws:s3:::x", ...]  // array
//
// StringList accepts both the single-string and array forms on unmarshal
// (vendor-supplied AWS policies use both interchangeably) and emits the
// AWS-canonical form on marshal — bare string for a one-element list, array
// otherwise. The internal representation is always []string so engine
// consumers (matchAction, matchResource) need no special-casing.
type StringList []string

// UnmarshalJSON accepts JSON of either form:
//
//	"foo"          → StringList{"foo"}
//	["foo","bar"]  → StringList{"foo", "bar"}
//	[]             → StringList{} (empty list)
//	null           → StringList{} (treated as empty)
//
// Any other shape (object, number, etc.) returns an error.
func (s *StringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = StringList{}
		return nil
	}
	// Array form — try first; cheap to detect.
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return fmt.Errorf("StringList: array element must be string: %w", err)
		}
		*s = arr
		return nil
	}
	// String form.
	if trimmed[0] == '"' {
		var single string
		if err := json.Unmarshal(data, &single); err != nil {
			return fmt.Errorf("StringList: invalid string: %w", err)
		}
		*s = StringList{single}
		return nil
	}
	return fmt.Errorf("StringList: expected JSON string or array of strings, got %s", string(data))
}

// MarshalJSON emits the AWS-canonical form. A one-element list is
// serialized as a bare string; longer lists serialize as arrays. Empty
// lists serialize as `[]` (not omitted) so consumers can tell intent
// apart from an explicit empty.
func (s StringList) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

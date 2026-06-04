package resource

import (
	"context"
	"encoding/json"
	"net/url"

	tea "charm.land/bubbletea/v2"
)

// fakeGateway is the minimal AdminClient stub for the cascade tests: it records the
// last admin call (so tests assert the method/path/body the view built) and returns
// a canned raw body / status / error. The real cascade only ever calls AdminRequest,
// so this one-method fake is the whole gateway surface it needs.
type fakeGateway struct {
	adminRaw    json.RawMessage
	adminStatus int
	err         error
	adminCalls  int
	lastAdmin   struct {
		method, path string
		query        url.Values
		body         any
	}
}

func (f *fakeGateway) AdminRequest(_ context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error) {
	f.adminCalls++
	f.lastAdmin.method, f.lastAdmin.path, f.lastAdmin.query, f.lastAdmin.body = method, path, query, body
	if f.err != nil {
		return nil, 0, f.err
	}
	status := f.adminStatus
	if status == 0 {
		status = 200
	}
	return f.adminRaw, status, nil
}

// keyRunes builds a key-press message from a literal string (single rune → keyed).
func keyRunes(s string) tea.KeyPressMsg {
	r := []rune(s)
	k := tea.KeyPressMsg{Text: s}
	if len(r) == 1 {
		k.Code = r[0]
	}
	return k
}

package consumer

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// isJSONNulPoison reports whether err is the permanent "null character in
// jsonb/text" data error. PostgreSQL raises SQLSTATE 22P05
// (untranslatable_character) for a `\u0000` escape inside jsonb and 22021
// (invalid_character_value_for_cast) for a raw NUL byte. Both are permanent —
// retrying the same bytes will fail forever — so the offending row is skipped
// rather than redelivered.
//
// The SQLSTATE is read from the TYPED *pgconn.PgError carried in the error chain
// (errors.As traverses fmt.Errorf wrapping), not by substring-matching
// err.Error(): a string match both false-triggers on a payload that
// merely contains "22021" and misses a real 22021 wrapped so its message text no
// longer carries the literal code. pgx always surfaces DB errors as
// *pgconn.PgError, so the typed check is exact for every production path.
func isJSONNulPoison(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "22021" || pgErr.Code == "22P05"
	}
	return false
}

// stripNul removes PostgreSQL-illegal null bytes (\x00) from a string.
// PostgreSQL UTF-8 columns reject \x00 with SQLSTATE 22021.
func stripNul(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

func stripNulPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := stripNul(*p)
	return &s
}

// jsonNulEscape is the 6-character literal sequence \u0000 (backslash, 'u',
// four zeros) that JSON uses to encode U+0000. PostgreSQL's jsonb type rejects
// it with SQLSTATE 22P05 (untranslatable_character) even though it is valid
// JSON — unlike text columns, jsonb cannot store a NUL code point in any form.
// Stripping the raw \x00 byte alone is NOT enough: a marshaller that escaped
// the NUL (e.g. encoding/json on a Go string containing \x00) emits this
// 6-char sequence, which sails past a raw-byte filter and poisons the insert.
const jsonNulEscape = `\u0000`

// stripNulJSON removes both forms of the PostgreSQL-illegal NUL from a JSON
// payload before it is bound to a jsonb column: the raw \x00 byte (SQLSTATE
// 22021) AND the 6-char \u0000 escape sequence (SQLSTATE 22P05). Either form
// is a permanent insert failure, so they are stripped at the source rather
// than dead-lettered.
func stripNulJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	s := string(raw)
	hasRawNul := strings.ContainsRune(s, 0)
	hasEscaped := strings.Contains(s, jsonNulEscape)
	if !hasRawNul && !hasEscaped {
		return raw
	}
	if hasRawNul {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if hasEscaped {
		s = strings.ReplaceAll(s, jsonNulEscape, "")
	}
	return json.RawMessage(s)
}

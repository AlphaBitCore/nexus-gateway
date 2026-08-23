// Package semantic — keys.go carries the identity axis of the L2 entry: which
// HASH key an entry occupies, and which validity tag decides whether a lookup
// may reach it. Both compose the same inputs from opposite directions, and
// both are called by the write path AND the read path, so they live together
// rather than beside the Valkey command plumbing in client.go.
//
// Vector encoding sits here too: it is the third thing derived from an entry's
// inputs rather than issued to Valkey.
package semantic

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Key helpers

// entryKey returns the Redis HASH key for an L2 entry. The hash folds the
// embedding input together with the entry's scope (vk_scope), response kind,
// and — unless AllowCrossModel is set — its upstream provider + model, so
// that logically-distinct entries that happen to share embedding text do not
// collide on a single key and evict each other via HSET overwrite.
//
// A NUL separator delimits the components so concatenation is unambiguous
// (no value can contain a NUL byte). When AllowCrossModel is true the model
// is interchangeable for retrieval, so provider+model are omitted and the
// newest response for a given (input, scope, kind) supersedes the prior one.
func entryKey(indexName string, in StoreInput) string {
	var sb strings.Builder
	sb.WriteString(in.EmbeddingInput)
	sb.WriteByte(0)
	sb.WriteString(in.VKScope)
	sb.WriteByte(0)
	sb.WriteString(in.ResponseKind)
	// AnswerKey belongs here as well as in the validity tag. The tag alone
	// keeps a temperature=0 lookup from RETRIEVING a temperature=2 entry, but
	// without it in the HASH key the two would share one key and overwrite
	// each other on HSET — so one of the two would be permanently missing and
	// the fix would read as a cache that never warms.
	if in.AnswerKey != "" {
		sb.WriteByte(0)
		sb.WriteString(in.AnswerKey)
	}
	if !in.AllowCrossModel {
		sb.WriteByte(0)
		sb.WriteString(in.UpstreamProvider)
		sb.WriteByte(0)
		sb.WriteString(in.UpstreamModel)
	}
	h := sha256.Sum256([]byte(sb.String()))
	hex := fmt.Sprintf("%x", h)
	return indexName + ":" + hex[:keyHashLen]
}

// Vector encoding helpers

// float32sToBytes encodes []float32 as FLOAT32 little-endian bytes.
// This matches the binary blob format expected by valkey-search.
func float32sToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// no answer-affecting parameters — the bulk of the corpus — keep reaching
// entries written before this existed.
//
// The two components are hashed rather than concatenated with a separator.
// A printable separator is ambiguous: with "~", the pair ("a~b", "c") and the
// pair ("a", "b~c") both render as "a~b~c", so two requests with genuinely
// different validity conditions would share a tag — the exact collision this
// function exists to prevent. NUL cannot occur in either component, so the
// hashed form is unambiguous, and hex output keeps the value safe to embed in
// a Valkey TAG query. The "k" prefix keeps a composed tag from ever colliding
// with a bare fingerprint.
func validityTag(fingerprint, answerKey string) string {
	if answerKey == "" {
		return fingerprint
	}
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + answerKey))
	return fmt.Sprintf("k%x", sum[:16])
}

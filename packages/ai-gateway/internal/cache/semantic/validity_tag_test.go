package semantic

import "testing"

// validityTag is the single composition point for the `fingerprint` TAG that
// both the write and the read path use. Anything that lets the two sides
// disagree turns L2 into a permanent 100% miss, so the properties below are
// about agreement first and separation second.

func TestValidityTag_EmptyAnswerKeyLeavesFingerprintUntouched(t *testing.T) {
	if got := validityTag("fp-abc", ""); got != "fp-abc" {
		t.Errorf("tag=%q, want the bare fingerprint so entries written before answer keys existed stay reachable", got)
	}
}

func TestValidityTag_DifferentAnswerKeysDoNotShareATag(t *testing.T) {
	a := validityTag("fp-abc", "key-deterministic")
	b := validityTag("fp-abc", "key-creative")
	if a == b {
		t.Error("two answer keys produced one tag; the FT.SEARCH filter would return either entry for either request")
	}
	if a == "fp-abc" || b == "fp-abc" {
		t.Error("a non-empty answer key must change the tag, or the filter cannot separate the requests")
	}
}

// A config rotation must still invalidate everything, answer key or not.
func TestValidityTag_FingerprintStillSeparates(t *testing.T) {
	if validityTag("fp-old", "k") == validityTag("fp-new", "k") {
		t.Error("a config fingerprint change no longer invalidates entries")
	}
}

// The separator must not let one component's value impersonate a different
// split of the two.
func TestValidityTag_ComponentsCannotBeConfused(t *testing.T) {
	if validityTag("a~b", "c") == validityTag("a", "b~c") {
		t.Error("tag composition is ambiguous: two distinct (fingerprint, answerKey) pairs collide")
	}
}

// entryKey folds the answer key too. Without that the two variants share one
// HASH key and overwrite each other on HSET, so one would be permanently
// missing and the filter fix would read as a cache that never warms.
func TestEntryKey_AnswerKeySeparatesHashKeys(t *testing.T) {
	base := StoreInput{
		EmbeddingInput:   "what is the capital of France",
		VKScope:          "v1:vk:1",
		ResponseKind:     "response",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
	}
	withA := base
	withA.AnswerKey = "temp0"
	withB := base
	withB.AnswerKey = "temp2"

	if entryKey("idx", withA) == entryKey("idx", withB) {
		t.Error("two answer keys share a HASH key; one entry would evict the other on every write")
	}
	if entryKey("idx", base) == entryKey("idx", withA) {
		t.Error("an entry with no answer key shares a HASH key with one that has it")
	}
}

// Requests without answer-affecting parameters must keep their historical key
// so the bulk of the corpus is not cold-started.
func TestEntryKey_EmptyAnswerKeyIsStable(t *testing.T) {
	in := StoreInput{
		EmbeddingInput:   "hello",
		VKScope:          "v1:vk:1",
		ResponseKind:     "response",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
	}
	withEmpty := in
	withEmpty.AnswerKey = ""
	if entryKey("idx", in) != entryKey("idx", withEmpty) {
		t.Error("an explicit empty answer key changed the entry key; default-parameter traffic would cold-start")
	}
}

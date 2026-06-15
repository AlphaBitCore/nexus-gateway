package hashchain

import (
	"errors"
	"strings"
	"testing"
)

// mkLink folds one authentic link the way every chained writer does:
// canonicalize the envelope, chain over the previous hash.
func mkLink(t *testing.T, prev *string, seq int, envelope string) Link {
	t.Helper()
	hashInput, err := Canonicalize([]byte(envelope))
	if err != nil {
		t.Fatal(err)
	}
	return Link{Seq: seq, PrevHash: prev, Hash: ChainHash(prev, hashInput), HashInput: hashInput}
}

func threeLinks(t *testing.T) []Link {
	t.Helper()
	l1 := mkLink(t, nil, 1, `{"seq":1,"v":"a"}`)
	l2 := mkLink(t, &l1.Hash, 2, `{"seq":2,"v":"b"}`)
	l3 := mkLink(t, &l2.Hash, 3, `{"seq":3,"v":"c"}`)
	return []Link{l1, l2, l3}
}

// TestChainHash_FoldsPrevHash pins the recipe: the same input under a
// different prev hash yields a different link hash (each entry covers its
// predecessor), and a nil prev is the genesis case.
func TestChainHash_FoldsPrevHash(t *testing.T) {
	input := []byte(`{"seq":1}`)
	genesis := ChainHash(nil, input)
	if len(genesis) != 64 {
		t.Fatalf("hash must be hex SHA-256 (64 chars), got %d", len(genesis))
	}
	prev := "abc"
	if ChainHash(&prev, input) == genesis {
		t.Fatal("the previous hash must be folded into the link hash")
	}
}

// TestVerifyLinks_AcceptsAuthenticChain: an unmodified chain verifies.
func TestVerifyLinks_AcceptsAuthenticChain(t *testing.T) {
	if err := VerifyLinks(threeLinks(t)); err != nil {
		t.Fatalf("authentic chain must verify: %v", err)
	}
	if err := VerifyLinks(nil); err != nil {
		t.Fatalf("an empty chain is trivially intact: %v", err)
	}
}

// TestVerifyLinks_NamesEveryBreak: each tamper class fails with
// ErrChainBroken and names the offending seq.
func TestVerifyLinks_NamesEveryBreak(t *testing.T) {
	t.Run("modified envelope", func(t *testing.T) {
		links := threeLinks(t)
		links[1].HashInput = []byte(`{"seq":2,"v":"FORGED"}`)
		err := VerifyLinks(links)
		if !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "hash mismatch at seq 2") {
			t.Fatalf("err = %v, want chain-broken naming seq 2", err)
		}
	})
	t.Run("deleted middle entry (seq gap)", func(t *testing.T) {
		links := threeLinks(t)
		err := VerifyLinks([]Link{links[0], links[2]})
		if !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "seq gap at 2") {
			t.Fatalf("err = %v, want chain-broken naming the gap at 2", err)
		}
	})
	t.Run("relinked prevHash", func(t *testing.T) {
		links := threeLinks(t)
		other := "0000"
		links[2].PrevHash = &other
		err := VerifyLinks(links)
		if !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "prevHash mismatch at seq 3") {
			t.Fatalf("err = %v, want chain-broken naming seq 3", err)
		}
	})
	t.Run("genesis with a prevHash", func(t *testing.T) {
		links := threeLinks(t)[:1]
		ghost := "ffff"
		links[0].PrevHash = &ghost
		err := VerifyLinks(links)
		if !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "prevHash mismatch at seq 1") {
			t.Fatalf("err = %v, want chain-broken at seq 1", err)
		}
	})
}

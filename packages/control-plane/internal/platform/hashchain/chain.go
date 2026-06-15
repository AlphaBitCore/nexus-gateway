package hashchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// chain.go — the single tamper-evidence chain recipe shared by every chained
// audit log on the platform; chained tables fold links with ChainHash over a
// canonical envelope (Canonicalize) and verify with VerifyLinks. The envelope
// bytes (HashInput) are stored verbatim at write time, which makes
// verification independent of spilled, swept, or mutable content.

// ErrChainBroken marks a verification failure; every VerifyLinks error wraps
// it with the offending seq.
var ErrChainBroken = errors.New("hash chain broken")

// ChainHash folds one link: SHA256(prevHash ‖ hashInput), hex. prev is nil
// only for the first link of a chain.
func ChainHash(prev *string, hashInput []byte) string {
	hsh := sha256.New()
	if prev != nil {
		hsh.Write([]byte(*prev))
	}
	hsh.Write(hashInput)
	return hex.EncodeToString(hsh.Sum(nil))
}

// Link is one generic chain entry: any chained table projects its rows onto
// this shape for verification.
type Link struct {
	Seq       int
	PrevHash  *string
	Hash      string
	HashInput []byte
}

// VerifyLinks walks a chain: Seq must be 1..n gapless, each PrevHash must
// equal the prior Hash (NULL only at seq 1), and each Hash must recompute
// from (prev, HashInput). Every failure wraps ErrChainBroken with the
// offending seq so callers can name the break.
func VerifyLinks(links []Link) error {
	var prev *string
	for i := range links {
		l := &links[i]
		if l.Seq != i+1 {
			return fmt.Errorf("%w: seq gap at %d (have %d)", ErrChainBroken, i+1, l.Seq)
		}
		if (l.PrevHash == nil) != (prev == nil) || (prev != nil && l.PrevHash != nil && *prev != *l.PrevHash) {
			return fmt.Errorf("%w: prevHash mismatch at seq %d", ErrChainBroken, l.Seq)
		}
		if ChainHash(prev, l.HashInput) != l.Hash {
			return fmt.Errorf("%w: hash mismatch at seq %d", ErrChainBroken, l.Seq)
		}
		prev = &links[i].Hash
	}
	return nil
}

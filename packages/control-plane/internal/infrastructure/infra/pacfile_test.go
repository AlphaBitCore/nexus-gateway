package infra

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// renderPAC renders the shipped template directly, so these tests cover the
// artifact rather than the handler around it.
func renderPAC(t *testing.T, fragments []string, failOpen bool) string {
	t.Helper()
	var buf bytes.Buffer
	err := pacFileTmpl.Execute(&buf, struct {
		Fragments []string
		ProxyHost string
		ProxyPort string
		FailOpen  bool
	}{Fragments: fragments, ProxyHost: "proxy.corp", ProxyPort: "3128", FailOpen: failOpen})
	if err != nil {
		t.Fatalf("render PAC: %v", err)
	}
	return buf.String()
}

// seededFragments builds n fragments in the two shapes the handler produces.
func seededFragments(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		host := fmt.Sprintf("api%d.example.com", i)
		if i%2 == 0 {
			out = append(out, fmt.Sprintf(`dnsDomainIs(host, %q)`, "."+host))
		} else {
			// The exact-host shape, which contains a `||` of its own — the
			// reason each fragment is parenthesised when joined.
			out = append(out, fmt.Sprintf(`host === %q || dnsDomainIs(host, ".%s")`, host, host))
		}
	}
	if n == 0 {
		return []string{"false"} // what the handler substitutes for no domains
	}
	return out
}

// TestPACFile_IsOneConditionNotOneIfPerDomain is the defect.
//
// A template emitting one `if` per domain and joining them with `||` BETWEEN
// the statements gives a file with N domains N-1 stray `||`. A PAC file is
// JavaScript a client's proxy resolver parses, and it rejects the WHOLE file on
// a syntax error — the seed ships 63 enabled domains, so every deployment's
// download would be unusable. Exactly one domain parses, which is how it hides.
//
// The assertion is structural rather than a substring match: the pre-existing
// happy-path test asserted three substrings that are all present in a file that
// does not parse.
func TestPACFile_IsOneConditionNotOneIfPerDomain(t *testing.T) {
	for _, n := range []int{0, 1, 2, 63} {
		for _, failOpen := range []bool{false, true} {
			t.Run(fmt.Sprintf("domains=%d failOpen=%v", n, failOpen), func(t *testing.T) {
				out := renderPAC(t, seededFragments(n), failOpen)

				if got := strings.Count(out, "if ("); got != 1 {
					t.Errorf("%d `if (` in the file; a PAC file must test every domain in ONE condition\n%s", got, out)
				}
				// A `||` may only appear INSIDE the condition line. One on its
				// own, or trailing a line, is the shipped syntax error.
				for i, line := range strings.Split(out, "\n") {
					trimmed := strings.TrimSpace(line)
					if trimmed == "||" || strings.HasSuffix(trimmed, "||") {
						t.Errorf("line %d is a dangling `||`: %q\n%s", i+1, line, out)
					}
				}
			})
		}
	}
}

// TestPACFile_FailOpenSitsOnTheMatchedReturn pins the second defect. failOpen
// on the FALLTHROUGH sends every UNMONITORED request through
// the proxy while monitored domains keep the strict directive — the inverse of
// what the flag means. "A monitored domain may fall back to DIRECT when the
// proxy is unreachable" is a statement about the matched branch.
func TestPACFile_FailOpenSitsOnTheMatchedReturn(t *testing.T) {
	frags := seededFragments(2)

	strict := renderPAC(t, frags, false)
	matched, fallthrough_ := returnsOf(t, strict)
	if matched != `return "PROXY proxy.corp:3128";` {
		t.Errorf("strict matched return = %q", matched)
	}
	if fallthrough_ != `return "DIRECT";` {
		t.Errorf("strict fallthrough = %q; an unmonitored host must go DIRECT", fallthrough_)
	}

	open := renderPAC(t, frags, true)
	matched, fallthrough_ = returnsOf(t, open)
	if !strings.Contains(matched, "; DIRECT") {
		t.Errorf("failOpen matched return = %q; the fallback belongs on the MONITORED branch", matched)
	}
	if fallthrough_ != `return "DIRECT";` {
		t.Errorf("failOpen fallthrough = %q; failOpen must not route unmonitored traffic through the proxy", fallthrough_)
	}
}

// returnsOf extracts the two return statements in source order: the one guarded
// by the `if`, and the fallthrough.
func returnsOf(t *testing.T, pac string) (matched, fallthrough_ string) {
	t.Helper()
	var returns []string
	for _, line := range strings.Split(pac, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "return ") {
			returns = append(returns, trimmed)
		}
	}
	if len(returns) != 2 {
		t.Fatalf("want exactly 2 return statements, got %d:\n%s", len(returns), pac)
	}
	return returns[0], returns[1]
}

// TestPACFile_ParsesAsJavaScript is the end-to-end check: the artifact is
// JavaScript, so a real parser is the only honest oracle for "is it valid".
//
// Skipped when node is unavailable, which is why it is NOT the primary gate —
// the structural assertions above encode the same property deterministically.
func TestPACFile_ParsesAsJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; the structural assertions above are the gate")
	}
	for _, n := range []int{0, 1, 2, 63} {
		for _, failOpen := range []bool{false, true} {
			t.Run(fmt.Sprintf("domains=%d failOpen=%v", n, failOpen), func(t *testing.T) {
				pac := renderPAC(t, seededFragments(n), failOpen)
				// `new Function(body)` parses without executing.
				cmd := exec.Command(node, "-e",
					`const src=require('fs').readFileSync(0,'utf8'); new Function(src); `+
						`if(typeof src!=='string'||!src.includes('FindProxyForURL'))process.exit(2);`)
				cmd.Stdin = strings.NewReader(pac)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("the generated PAC file is not valid JavaScript: %v\n%s\n--- file ---\n%s", err, out, pac)
				}
			})
		}
	}
}

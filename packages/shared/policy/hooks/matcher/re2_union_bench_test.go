package matcher

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// realRulePatterns is a sample of the rule-pack patterns this deployment actually
// ships (24 of 423 seeded rules, taken verbatim). Benchmarking the RE2 matcher on
// invented patterns would measure the wrong thing: nearly every real rule is
// case-insensitive, which is where Go's regexp spends its time (unicode.SimpleFold
// was 6% of whole-process CPU under live load).
var realRulePatterns = []string{
	`(?i)\bATTORNEY[ -](?:CLIENT (?:PRIVILEGED?|COMMUNICATION)|WORK PRODUCT)\b`,
	`(?i)\b(?:how\s+(?:to|do\s+i)|step[-\s]?by[-\s]?step)\s+(?:synthesi[sz]e|make|prepare)\s+(?:tatp|hmtd|nitroglycerin|c-?4|semtex|thermite)\b`,
	`(?i)\b[\w.-]{1,60}\.(?:pem|p12|pfx|jks|keystore|key|crt|cer)\b`,
	`\btfp_[A-Za-z0-9_-]{44,59}\b`,
	`(?i)repeat\s+(the\s+)?(words?|text|everything)\s+(above|before\s+this)\s+(verbatim|exactly|word\s+for\s+word)`,
	`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|mariadb|mssql|sqlserver)://[^\s/@]{1,80}@(?:[\w.-]{1,60}\.(?:internal|corp|local|lan|intra)|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})`,
	`(?i)\b(?:do not sell (?:or share )?my (?:personal )?(?:information|data|info)|right to opt[- ]out of (?:the )?sale|CCPA|CPRA|California Consumer Privacy Act|California Privacy Rights Act)\b`,
	`(?i)forget\s+(everything|all)\s+(you\s+were\s+told|above|before)`,
	`(?i)(?:os\.environ|process\.env|getenv)\s*\[\s*["'](?:AWS_|OPENAI_|ANTHROPIC_|GITHUB_|STRIPE_)`,
	`(?i)\b(?:white\s+power|white\s+supremac(?:y|ist)|14\s*words|sieg\s+heil|blood\s+and\s+soil)\b`,
	`\bhooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9+/]{43,56}\b`,
	`(?i)(developer|admin|root|god|debug)\s+mode\s+(on|enabled|active|unlocked)\b\s*[.!,;:]`,
	`(?i)\bvssadmin(?:\.exe)?\s+delete\s+shadows\b`,
	`(?i)\b(?:how\s+(?:to|do\s+i)|step[-\s]?by[-\s]?step|recipe\s+for)\s+(?:build|make|construct|assemble)\s+(?:a\s+)?(?:pipe\s+bomb|pressure[-\s]cooker\s+bomb|ied|improvised\s+explosive|nail\s+bomb|fertili[sz]er\s+bomb)\b`,
	`\bfigd_[A-Za-z0-9_-]{40,43}\b`,
	`\bFLWSECK_TEST-[a-h0-9]{32}-X\b`,
	`(?i)\b(?:execute|executemany|query)\s*\(\s*["'][^"']{0,160}["']{1,2}\s*\+`,
	`(?i)(?:write|open|create|append)[^\n]{0,40}?(?:/etc/(?:passwd|shadow|sudoers|ssh/|cron\.|hosts)|~/\.ssh/|C:\\Windows\\(?:System32|SysWOW64))`,
	`(?i)(?:%2e%2e[%/\\]|%252e%252e|\.\.%2f|\.\.%5c|\.{4,}/{2,})`,
	`(?i)\b(?:ssn|social\s+security(?:\s+(?:number|no\.?|#))?)\b\s*[:#]?\s*[0-9]{3}[ -]?[0-9]{2}[ -]?[0-9]{4}\b`,
	`\bCCIPAT_[A-Za-z0-9]{22}_[A-Za-z0-9]{40}\b`,
	`(?i)\b(?:how\s+(?:to|do\s+i)|best\s+way\s+to)\s+(?:murder|assassinate|kill)\s+(?:a\s+person|someone|him|her|them)\b`,
	`(?i)<!DOCTYPE[^>]{0,80}(?:SYSTEM|PUBLIC)\b`,
	`(?i)\bpip\d?\s+install\b[^\n]{0,120}--(?:index-url|extra-index-url)[=\s]`,
}

func benchPatterns(n int) []Pattern {
	out := make([]Pattern, 0, n)
	for i := range n {
		out = append(out, Pattern{ID: i, Expr: realRulePatterns[i%len(realRulePatterns)]})
	}
	return out
}

// benchBody builds a body of roughly size bytes of prose that no rule matches,
// optionally with one real match appended so the hit path is measured too.
func benchBody(size int, withHit bool) string {
	var b strings.Builder
	filler := "the quarterly planning notes mention nothing sensitive at all, just schedules and names. "
	for b.Len() < size {
		b.WriteString(filler)
	}
	if withHit {
		b.WriteString(" ATTORNEY-CLIENT PRIVILEGED ")
	}
	return b.String()
}

// unionScan is the candidate: one combined alternation answers "could anything
// match", and the per-pattern demux runs only when it does. Correctness rests on
// (?:a)|(?:b) matching iff a or b matches; a union that fails to compile falls
// back to the per-pattern loop, so the optimisation can never change results.
type unionScan struct {
	m     *re2Matcher
	union *regexp.Regexp
}

func newUnionScan(m *re2Matcher) *unionScan {
	parts := make([]string, 0, len(m.pats))
	for _, p := range m.pats {
		parts = append(parts, "(?:"+p.re.String()+")")
	}
	u, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return &unionScan{m: m}
	}
	return &unionScan{m: m, union: u}
}

func (u *unionScan) Scan(segments []string, firstOnly bool) []Hit {
	if u.union != nil {
		any := false
		for _, seg := range segments {
			if u.union.MatchString(seg) {
				any = true
				break
			}
		}
		if !any {
			return nil
		}
	}
	return u.m.Scan(segments, firstOnly)
}

// parallelScan is the second candidate: the per-pattern scans are independent, so
// fan them out over the available cores and merge by pattern ID. Total CPU is
// unchanged; the wall-clock a caller waits for is what drops. Results are
// order-normalised so they can be compared against the sequential scan.
type parallelScan struct{ m *re2Matcher }

func (p *parallelScan) Scan(segments []string, firstOnly bool) []Hit {
	workers := runtime.GOMAXPROCS(0)
	if workers > len(p.m.pats) {
		workers = len(p.m.pats)
	}
	if workers < 2 {
		return p.m.Scan(segments, firstOnly)
	}
	per := make([][]Hit, len(p.m.pats))
	var wg sync.WaitGroup
	next := make(chan int, len(p.m.pats))
	for i := range p.m.pats {
		next <- i
	}
	close(next)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				pt := p.m.pats[i]
				for si, seg := range segments {
					if firstOnly {
						if loc := pt.re.FindStringIndex(seg); loc != nil {
							per[i] = append(per[i], Hit{ID: pt.id, Seg: si, Start: loc[0], End: loc[1]})
						}
						continue
					}
					for _, loc := range pt.re.FindAllStringIndex(seg, -1) {
						per[i] = append(per[i], Hit{ID: pt.id, Seg: si, Start: loc[0], End: loc[1]})
					}
				}
			}
		}()
	}
	wg.Wait()
	var out []Hit
	for _, hs := range per {
		out = append(out, hs...)
	}
	return out
}

// BenchmarkRE2ScanSmall measures the cases where fanning out might COST rather
// than save: a small body against many patterns, and a large body against few.
// The fan-out threshold is set from these numbers instead of guessed.
func BenchmarkRE2ScanSmall(b *testing.B) {
	cases := []struct {
		bytes, pats int
	}{
		{512, 423}, {2 << 10, 423}, {16 << 10, 423},
		{400 << 10, 4}, {400 << 10, 16}, {16 << 10, 24},
		{512, 2}, {512, 4}, {512, 24}, {64, 423}, {64, 4},
	}
	for _, c := range cases {
		body := benchBody(c.bytes, false)
		m, _ := CompileRE2(benchPatterns(c.pats))
		re2 := m.(*re2Matcher)
		ps := &parallelScan{m: re2}
		label := fmt.Sprintf("bytes=%d/patterns=%d", c.bytes, c.pats)
		b.Run("perPattern/"+label, func(b *testing.B) {
			for b.Loop() {
				_ = re2.Scan([]string{body}, true)
			}
		})
		b.Run("parallel/"+label, func(b *testing.B) {
			for b.Loop() {
				_ = ps.Scan([]string{body}, true)
			}
		})
	}
}

func BenchmarkRE2Scan(b *testing.B) {
	for _, n := range []int{24, 100, 423} {
		for _, hit := range []bool{false, true} {
			body := benchBody(400<<10, hit)
			m, bad := CompileRE2(benchPatterns(n))
			if len(bad) != 0 {
				b.Fatalf("bad patterns: %v", bad)
			}
			re2 := m.(*re2Matcher)
			us := newUnionScan(re2)
			ps := &parallelScan{m: re2}
			label := fmt.Sprintf("patterns=%d/hit=%v", n, hit)

			b.Run("perPattern/"+label, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					_ = re2.Scan([]string{body}, true)
				}
			})
			b.Run("unionFirst/"+label, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					_ = us.Scan([]string{body}, true)
				}
			})
			b.Run("parallel/"+label, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					_ = ps.Scan([]string{body}, true)
				}
			})
		}
	}
}

// keep core imported for the PrescanPattern reference in the doc above
var _ = core.PrescanPattern{}

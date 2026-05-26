package helpers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricSnapshot captures Prometheus counter / gauge values at a point
// in time. Stored canonical-form so MetricDelta can do strict diffs.
type MetricSnapshot struct {
	at     time.Time
	values map[string]float64 // canonical: "name{k1="v1",k2="v2"}" (label keys sorted)
}

// At returns the wall-clock time the snapshot was taken — useful for
// scenario logs.
func (s *MetricSnapshot) At() time.Time { return s.at }

// ScrapeMetrics fetches the Prometheus text endpoint at the given base
// URL (e.g. http://localhost:3050) and returns a parsed snapshot. We
// only retain numeric counter / gauge samples — histogram buckets and
// summary quantiles are skipped (scenarios that need them should ask
// for the raw text endpoint separately). Times out after 5 s.
func ScrapeMetrics(ctx context.Context, baseURL string) (*MetricSnapshot, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s/metrics: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s/metrics: status %d body=%q", baseURL, resp.StatusCode, body)
	}
	snap := &MetricSnapshot{at: time.Now(), values: make(map[string]float64, 256)}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			parseMetricLine(strings.TrimRight(line, "\n"), snap.values)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read /metrics: %w", err)
		}
	}
	return snap, nil
}

// parseMetricLine handles one Prometheus textual sample. Skips help/type
// comment lines and anything that doesn't parse as `name{labels} value`
// or `name value`.
func parseMetricLine(line string, into map[string]float64) {
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	// Split on whitespace into NAME{labels} VALUE (timestamp ignored).
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return
	}
	val, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return
	}
	canonical := canonicaliseMetricKey(parts[0])
	if canonical == "" {
		return
	}
	into[canonical] = val
}

// canonicaliseMetricKey normalises label ordering so `name{a="1",b="2"}`
// and `name{b="2",a="1"}` resolve to the same key. Returns "" if the
// input is malformed.
func canonicaliseMetricKey(key string) string {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return key
	}
	close := strings.LastIndexByte(key, '}')
	if close < 0 || close <= open {
		return ""
	}
	name := key[:open]
	body := key[open+1 : close]
	if body == "" {
		return name
	}
	pairs := splitLabels(body)
	sort.Strings(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// splitLabels splits `a="1",b="2,3"` correctly — a comma inside a
// double-quoted value must not break the pair.
func splitLabels(body string) []string {
	out := []string{}
	depth := 0
	start := 0
	inQ := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case '"':
			if i > 0 && body[i-1] == '\\' {
				continue
			}
			inQ = !inQ
		case ',':
			if !inQ && depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out
}

// Counter returns the numeric value of the named series with EXACT
// label match, or 0 if not present. Use Diff for delta arithmetic.
func (s *MetricSnapshot) Counter(name string, labels map[string]string) float64 {
	return s.values[canonicaliseMetricKey(buildKey(name, labels))]
}

// CounterSum sums every series matching name with at least the given
// label subset (other labels free). Useful for "total acrooss all
// providers".
func (s *MetricSnapshot) CounterSum(name string, requiredLabels map[string]string) float64 {
	var total float64
	prefix := name
	if !strings.HasSuffix(prefix, "{") {
		prefix += "{"
	}
	for k, v := range s.values {
		if !strings.HasPrefix(k, prefix) && k != name {
			continue
		}
		if hasLabelSubset(k, requiredLabels) {
			total += v
		}
	}
	return total
}

func buildKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+`="`+v+`"`)
	}
	sort.Strings(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

func hasLabelSubset(canonical string, required map[string]string) bool {
	if len(required) == 0 {
		return true
	}
	open := strings.IndexByte(canonical, '{')
	if open < 0 {
		return false
	}
	body := canonical[open+1 : strings.LastIndexByte(canonical, '}')]
	pairs := splitLabels(body)
	for k, v := range required {
		needle := k + `="` + v + `"`
		found := false
		for _, p := range pairs {
			if p == needle {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Delta returns after - before for the named series + labels.
func Delta(before, after *MetricSnapshot, name string, labels map[string]string) float64 {
	return after.Counter(name, labels) - before.Counter(name, labels)
}

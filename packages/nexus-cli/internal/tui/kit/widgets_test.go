package kit

import (
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestClip(t *testing.T) {
	if got := Clip("short", 10); got != "short" {
		t.Fatalf("under-limit must pass through, got %q", got)
	}
	got := Clip("abcdefghij", 5)
	if []rune(got)[len([]rune(got))-1] != '…' || len([]rune(got)) != 5 {
		t.Fatalf("over-limit must truncate to n runes with an ellipsis, got %q", got)
	}
	// Rune-aware: a multibyte string is never cut mid-codepoint.
	if got := Clip("日本語テスト", 3); len([]rune(got)) != 3 {
		t.Fatalf("multibyte clip must count runes, got %q", got)
	}
}

func TestWrapText(t *testing.T) {
	if got := WrapText("anything", 0); got != "anything" {
		t.Fatalf("width<=0 must be a no-op, got %q", got)
	}
	got := WrapText("the quick brown fox jumps", 10)
	if !strings.Contains(got, "\n") {
		t.Fatalf("a long line must wrap at width, got %q", got)
	}
}

func TestRenderCursorList(t *testing.T) {
	row := func(i int) string { return "row" + string(rune('0'+i)) }
	// budget clamps to >=3; the cursor row is marked with the brand arrow.
	out := RenderCursorList(1, 0, 5, row)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("budget<3 must clamp to 3 rows, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "▸") {
		t.Fatalf("the cursor row must carry the arrow marker, got %q", lines[0])
	}
	// A cursor past the budget scrolls the window so the cursor stays visible.
	out = RenderCursorList(3, 4, 6, row)
	if !strings.Contains(out, "row4") || strings.Contains(out, "row0") {
		t.Fatalf("the window must scroll to keep the cursor visible, got %q", out)
	}
	// Fewer rows than the budget: the window clamps to n (no blank rows past the end).
	out = RenderCursorList(5, 0, 2, row)
	if n := len(strings.Split(out, "\n")); n != 2 {
		t.Fatalf("the window must clamp to n rows, got %d: %q", n, out)
	}
}

func TestSparklineChart(t *testing.T) {
	if SparklineChart(nil, "x", 10, 2) != "" {
		t.Fatal("no series must render nothing")
	}
	if SparklineChart([]core.SparklineBucket{{}}, "x", 2, 1) != "" {
		t.Fatal("too-narrow width must render nothing")
	}
	series := []core.SparklineBucket{
		{Values: map[string]float64{"req": 1}},
		{Values: map[string]float64{"req": 5}},
		{Values: map[string]float64{"req": 3}},
	}
	if SparklineChart(series, "req", 12, 2) == "" {
		t.Fatal("a real series must produce a chart")
	}
}

func TestTileAndDetailRow(t *testing.T) {
	if !strings.Contains(Tile("Requests", "42"), "42") {
		t.Fatal("tile must render its value")
	}
	dr := DetailRow("Provider", "Anthropic")
	if !strings.Contains(dr, "Provider") || !strings.Contains(dr, "Anthropic") {
		t.Fatalf("detail row must render label + value, got %q", dr)
	}
}

func TestRenderMarkdown(t *testing.T) {
	// Degrades safely: empty or too-narrow returns the source unchanged.
	if RenderMarkdown("", 80) != "" {
		t.Fatal("empty source must pass through")
	}
	if got := RenderMarkdown("**hi**", 4); got != "**hi**" {
		t.Fatalf("too-narrow width must return source unchanged, got %q", got)
	}
	// A real render strips the raw markdown markers.
	out := RenderMarkdown("# Heading\n\nsome **bold** text", 80)
	if out == "" || strings.Contains(out, "**bold**") {
		t.Fatalf("markdown must render (markers stripped), got %q", out)
	}
}

// TestRenderMarkdown_BrandTheme locks the brand re-skin so a regression back to the
// stock dark theme (cyan headings, yellow-on-purple H1, salmon code) is caught. The
// H1 banner must carry the brand-blue background and inline code the soft-blue fg.
func TestRenderMarkdown_BrandTheme(t *testing.T) {
	out := RenderMarkdown("# Title\n\nuse `code` here", 80)
	if !strings.Contains(out, "48;2;59;81;138") { // H1 background = Brand #3b518a
		t.Errorf("H1 must use the brand-blue banner background, got %q", out)
	}
	if !strings.Contains(out, "38;2;158;203;255") { // inline code fg = #9ecbff
		t.Errorf("inline code must use the soft-blue brand fg, got %q", out)
	}
	// The stock dark theme's yellow-on-purple H1 (fg 228 / bg 63) must be gone.
	if strings.Contains(out, "38;5;228") || strings.Contains(out, "48;5;63") {
		t.Errorf("stock yellow-on-purple H1 must not survive the re-skin, got %q", out)
	}
}

func TestTick(t *testing.T) {
	type marker struct{}
	cmd := Tick(time.Millisecond, marker{})
	if cmd == nil {
		t.Fatal("Tick must return a command")
	}
	if _, ok := cmd().(marker); !ok {
		t.Fatalf("the command must deliver the scheduled message, got %T", cmd())
	}
}

func TestFetchCtx(t *testing.T) {
	ctx, cancel := FetchCtx()
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("FetchCtx must bound the fetch with a deadline")
	}
}

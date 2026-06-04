package kit

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	gstyles "github.com/charmbracelet/glamour/styles"
)

// Brand markdown palette. The stock glamour "dark" theme renders headings in
// cyan, H1 as yellow-on-purple, and inline code as salmon — all of which clash
// with the AlphaBitCore institutional blue. brandMarkdownStyle re-skins the
// theme so finalized assistant answers read as calm, on-brand prose: neutral
// body text, blue headings/links, a quiet navy code chip, bold = brighter white.
// Reserving color for structure (headings, code, links) keeps the body legible —
// the same restraint that makes a good CLI chat comfortable to read at length.
var brandMarkdownStyle = buildBrandMarkdownStyle()

func sp(s string) *string { return &s }

func buildBrandMarkdownStyle() ansi.StyleConfig {
	const (
		body    = "#d7dbe6" // soft neutral prose — easy on the eyes for long answers
		bright  = "#e6e9f0" // bold/strong emphasis reads as brighter white, not a 2nd hue
		accent  = "#7e96d6" // headings + link text (a legible lift of the brand blue)
		accent2 = "#5a73c0" // link underline / image
		brandBg = "#3b518a" // H1 banner background (brand blue)
		codeFg  = "#9ecbff" // inline code — soft blue, distinct without shouting
		codeBg  = "#232a3d" // inline code chip — quiet navy
		muted   = "#9aa2b1" // de-emphasized (H6, image caption)
		line    = "#3a4151" // horizontal rule
	)

	s := gstyles.DarkStyleConfig // copy, then re-skin the color-bearing fields

	s.Document.Color = sp(body)
	s.Heading.Color = sp(accent) // H2–H5 inherit this unless they override
	s.Heading.Bold = boolPtr(true)
	// H1: a clean brand banner instead of the stock yellow-on-purple.
	s.H1.Color = sp(bright)
	s.H1.BackgroundColor = sp(brandBg)
	s.H6.Color = sp(muted)
	s.Strong.Color = sp(bright)
	s.Code.Color = sp(codeFg)
	s.Code.BackgroundColor = sp(codeBg)
	s.Link.Color = sp(accent2)
	s.LinkText.Color = sp(accent)
	s.Image.Color = sp(accent2)
	s.ImageText.Color = sp(muted)
	s.HorizontalRule.Color = sp(line)

	return s
}

func boolPtr(b bool) *bool { return &b }

// RenderMarkdown renders markdown source to ANSI-styled, width-wrapped text for
// the chat transcript. It degrades safely: on an empty source, a too-narrow
// width, or any renderer error it returns the source unchanged, so an assistant
// reply is never lost behind a rendering failure. The wrap width is reduced by a
// small margin because glamour's document style adds its own left/right padding;
// without the margin a long line could exceed the chat pane and wrap raggedly.
func RenderMarkdown(src string, width int) string {
	if strings.TrimSpace(src) == "" || width < 8 {
		return src
	}
	wrap := width - 2
	// Use the explicit brand StyleConfig rather than WithAutoStyle: auto keys off
	// whether os.Stdout is a TTY (not the Bubble Tea alt-screen render target), so it
	// silently degrades to the no-op "notty" style — leaving raw `**` / `#` in the
	// transcript. The brand theme is dark-tuned; its 256/truecolor output renders
	// acceptably on light terminals too.
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(brandMarkdownStyle),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		return src
	}
	out, err := r.Render(src)
	if err != nil {
		return src
	}
	// Glamour brackets the document with blank lines; trim them so the transcript
	// stays compact and the tail-trim budget isn't spent on padding.
	return strings.Trim(out, "\n")
}

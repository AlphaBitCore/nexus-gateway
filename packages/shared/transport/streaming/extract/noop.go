package extract

// noopExtractor is the fallback returned by `Registry.Get` for unregistered
// adapter ids (non-LLM domains, unknown providers). It returns empty
// content for every input so a hook bound to "any" content can run safely
// against any traffic shape.
type noopExtractor struct{}

func (noopExtractor) ID() string                               { return "noop" }
func (noopExtractor) ExtractRequest(_ []byte) ExtractedContent { return ExtractedContent{} }
func (noopExtractor) NewAccumulator() Accumulator              { return &noopAccumulator{} }

type noopAccumulator struct {
	truncated bool
}

func (n *noopAccumulator) Feed(_ []byte) ExtractedDelta { return ExtractedDelta{} }
func (n *noopAccumulator) Snapshot() ExtractedContent {
	return ExtractedContent{Truncated: n.truncated}
}
func (n *noopAccumulator) Truncate() { n.truncated = true }

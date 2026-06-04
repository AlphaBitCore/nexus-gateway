package openapigen

import "gopkg.in/yaml.v3"

// omap is an insertion-ordered string-keyed map. OpenAPI documents and JSON
// Schemas must serialise with stable key order so regenerated specs produce
// minimal, reviewable diffs; Go's built-in map randomises iteration order, so
// the generator builds every object through omap instead.
type omap struct {
	keys []string
	vals map[string]any
}

// newOMap returns an empty ordered map ready for Set.
func newOMap() *omap { return &omap{vals: map[string]any{}} }

// Set inserts or overwrites key. First insertion fixes the serialisation
// position; overwriting an existing key keeps its original position.
func (m *omap) Set(key string, val any) *omap {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = val
	return m
}

// SetIf calls Set only when cond holds, so optional fields can be chained
// without intermediate if-blocks.
func (m *omap) SetIf(cond bool, key string, val any) *omap {
	if cond {
		m.Set(key, val)
	}
	return m
}

// Get returns the value stored at key and whether it was present.
func (m *omap) Get(key string) (any, bool) {
	v, ok := m.vals[key]
	return v, ok
}

// Len reports the number of keys.
func (m *omap) Len() int { return len(m.keys) }

// MarshalYAML renders the map as a YAML mapping node preserving insertion order.
func (m *omap) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range m.keys {
		keyNode := &yaml.Node{}
		if err := keyNode.Encode(k); err != nil {
			return nil, err
		}
		valNode := &yaml.Node{}
		if err := valNode.Encode(m.vals[k]); err != nil {
			return nil, err
		}
		node.Content = append(node.Content, keyNode, valNode)
	}
	return node, nil
}

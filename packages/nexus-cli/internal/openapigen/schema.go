package openapigen

import (
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// schemaBuilder converts Go types into OpenAPI 3.1 JSON Schema objects. Named
// struct types are hoisted into a shared component map and referenced by
// $ref so the same model is described once per document; anonymous structs
// (inline request bodies) are emitted in place.
type schemaBuilder struct {
	// components maps a component name to its schema object.
	components map[string]*omap
	// compName maps a type's full identity (pkgPath\x00name) to the component
	// name it was registered under. It both guards against infinite recursion on
	// self-referential types and lets a name collision between two distinct types
	// that share a last path segment (a/cache.Config vs b/cache.Config) be detected
	// and disambiguated rather than silently aliased.
	compName map[string]string
	// order records component insertion order for deterministic output.
	order []string
}

func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{components: map[string]*omap{}, compName: map[string]string{}}
}

// componentNames returns the registered component names in a stable order.
func (b *schemaBuilder) componentNames() []string {
	out := append([]string(nil), b.order...)
	sort.Strings(out)
	return out
}

// schemaFor renders t as a JSON Schema object. nullable marks the schema as
// permitting null (used for pointer fields).
func (b *schemaBuilder) schemaFor(t types.Type) *omap {
	if t == nil {
		return newOMap() // empty schema == "any"
	}
	switch u := t.(type) {
	case *types.Pointer:
		// A pointer is the element schema plus null. OpenAPI 3.1 uses a JSON
		// Schema type array for nullability.
		return withNull(b.schemaFor(u.Elem()))
	case *types.Named:
		return b.namedSchema(u)
	case *types.Alias:
		return b.schemaFor(types.Unalias(u))
	case *types.Basic:
		return basicSchema(u)
	case *types.Slice:
		return b.arraySchema(u.Elem())
	case *types.Array:
		return b.arraySchema(u.Elem())
	case *types.Map:
		s := newOMap().Set("type", "object")
		s.Set("additionalProperties", b.schemaFor(u.Elem()))
		return s
	case *types.Struct:
		return b.structSchema(u)
	case *types.Interface:
		return newOMap() // any
	default:
		return newOMap().Set("x-nexus-unresolved-type", t.String())
	}
}

// namedSchema handles defined types: well-known stdlib types map to formats,
// json.RawMessage maps to "any", struct-backed types are hoisted to a
// component $ref, and everything else recurses into its underlying type.
func (b *schemaBuilder) namedSchema(n *types.Named) *omap {
	full := n.Obj().Pkg()
	pkgPath := ""
	if full != nil {
		pkgPath = full.Path()
	}
	name := n.Obj().Name()

	switch {
	case pkgPath == "time" && name == "Time":
		return newOMap().Set("type", "string").Set("format", "date-time")
	case pkgPath == "time" && name == "Duration":
		return newOMap().Set("type", "integer").Set("description", "nanoseconds")
	case pkgPath == "encoding/json" && name == "RawMessage":
		return newOMap().Set("description", "arbitrary JSON value")
	}

	switch n.Underlying().(type) {
	case *types.Struct:
		full := pkgPath + "\x00" + name // the type's unique identity
		comp, known := b.compName[full]
		if !known {
			comp = b.claimComponent(pkgPath, name)
			b.compName[full] = comp
			b.order = append(b.order, comp)
			// Reserve the name (a nil placeholder) BEFORE recursing so a self- or
			// mutual reference resolves to the $ref instead of recursing forever, and
			// so a sibling type that wants the same name sees it taken and disambiguates.
			b.components[comp] = nil
			b.components[comp] = b.structSchema(n.Underlying().(*types.Struct))
		}
		return newOMap().Set("$ref", "#/components/schemas/"+comp)
	default:
		return b.schemaFor(n.Underlying())
	}
}

// claimComponent returns a component name for (pkgPath, name) that is not already
// taken by a DIFFERENT type. The common case (a flat layout with unique last
// segments) returns componentName unchanged; a genuine collision (two types whose
// package last-segment + name match) gets a numeric suffix (cache_Config,
// cache_Config_2, …) so the second type is described, not silently aliased to the first.
func (b *schemaBuilder) claimComponent(pkgPath, name string) string {
	base := componentName(pkgPath, name)
	comp := base
	for n := 2; b.taken(comp); n++ {
		comp = base + "_" + strconv.Itoa(n)
	}
	return comp
}

// taken reports whether a component name is already reserved (value may be a nil
// placeholder during recursive construction; key presence is what matters).
func (b *schemaBuilder) taken(comp string) bool {
	_, ok := b.components[comp]
	return ok
}

// arraySchema renders a list type. []byte becomes a base64 string per JSON
// conventions; every other element type recurses.
func (b *schemaBuilder) arraySchema(elem types.Type) *omap {
	if basic, ok := elem.(*types.Basic); ok && basic.Kind() == types.Byte {
		return newOMap().Set("type", "string").Set("format", "byte")
	}
	return newOMap().Set("type", "array").Set("items", b.schemaFor(elem))
}

// structSchema renders a struct as an object schema, honouring json tags for
// property names and `omitempty` / pointer-ness for the required set.
func (b *schemaBuilder) structSchema(s *types.Struct) *omap {
	props := newOMap()
	var required []string
	for i := 0; i < s.NumFields(); i++ {
		f := s.Field(i)
		if !f.Exported() && !f.Embedded() {
			continue
		}
		name, omitempty, skip := jsonField(reflect.StructTag(s.Tag(i)), f.Name())
		if skip {
			continue
		}
		if f.Embedded() && name == f.Name() {
			// Promote embedded struct fields inline when there is no json tag.
			if st, ok := derefStruct(f.Type()); ok {
				inlineStruct(props, &required, b, st)
				continue
			}
		}
		b.addField(props, &required, name, f.Type(), omitempty)
	}
	out := newOMap().Set("type", "object")
	if props.Len() > 0 {
		out.Set("properties", props)
	}
	if len(required) > 0 {
		out.Set("required", required)
	}
	return out
}

// inlineStruct promotes an embedded struct's fields into the parent property
// set (Go struct embedding flattens into the JSON object).
func inlineStruct(props *omap, required *[]string, b *schemaBuilder, s *types.Struct) {
	for i := 0; i < s.NumFields(); i++ {
		f := s.Field(i)
		if !f.Exported() {
			continue
		}
		name, omitempty, skip := jsonField(reflect.StructTag(s.Tag(i)), f.Name())
		if skip {
			continue
		}
		b.addField(props, required, name, f.Type(), omitempty)
	}
}

// addField sets one property's schema and, when it is neither a pointer nor
// omitempty, records it as required — the shared rule used by both the direct
// struct fields and the inlined embedded ones.
func (b *schemaBuilder) addField(props *omap, required *[]string, name string, t types.Type, omitempty bool) {
	props.Set(name, b.schemaFor(t))
	if _, isPtr := t.(*types.Pointer); !isPtr && !omitempty {
		*required = append(*required, name)
	}
}

func derefStruct(t types.Type) (*types.Struct, bool) {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		t = n.Underlying()
	}
	s, ok := t.(*types.Struct)
	return s, ok
}

// basicSchema maps a Go basic kind to its JSON Schema type + format.
func basicSchema(b *types.Basic) *omap {
	s := newOMap()
	switch b.Kind() {
	case types.Bool:
		s.Set("type", "boolean")
	case types.String:
		s.Set("type", "string")
	case types.Int, types.Int8, types.Int16, types.Int32,
		types.Uint, types.Uint8, types.Uint16, types.Uint32:
		s.Set("type", "integer")
	case types.Int64, types.Uint64:
		s.Set("type", "integer").Set("format", "int64")
	case types.Float32, types.Float64:
		s.Set("type", "number")
	default:
		s.Set("x-nexus-unresolved-basic", b.String())
	}
	return s
}

// withNull turns a schema into a nullable one by promoting its single "type"
// into a [type, "null"] array, the OpenAPI 3.1 nullability form. Schemas
// without a concrete type (e.g. $ref) are returned unchanged.
func withNull(s *omap) *omap {
	if t, ok := s.Get("type"); ok {
		if ts, isStr := t.(string); isStr {
			s.Set("type", []string{ts, "null"})
		}
	}
	return s
}

// jsonField parses a struct field's json tag, returning the serialised name,
// whether omitempty is set, and whether the field is skipped (`json:"-"`).
func jsonField(tag reflect.StructTag, fieldName string) (name string, omitempty, skip bool) {
	raw, ok := tag.Lookup("json")
	if !ok {
		return fieldName, false, false
	}
	parts := strings.Split(raw, ",")
	if parts[0] == "-" && len(parts) == 1 {
		return "", false, true
	}
	name = parts[0]
	if name == "" {
		name = fieldName
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}

// componentName derives a stable, collision-resistant component key from a
// type's package path + name (e.g. "quota.Policy" -> "quota_Policy").
func componentName(pkgPath, name string) string {
	if pkgPath == "" {
		return name
	}
	seg := pkgPath
	if i := strings.LastIndex(pkgPath, "/"); i >= 0 {
		seg = pkgPath[i+1:]
	}
	return seg + "_" + name
}

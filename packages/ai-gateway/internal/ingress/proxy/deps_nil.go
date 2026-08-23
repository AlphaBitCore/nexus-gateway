package proxy

import "reflect"

// unboxNilDeps rewrites every interface field of Deps that holds a typed nil
// into an untyped nil, and reports which fields it changed.
//
// Most fields on Deps document a nil as meaning "this dependency is absent" —
// the proxy skips L2 when SemanticReader is nil, skips cache-cost accounting
// when CachePricing is nil, and so on. Assigning a nil *T to such a field does
// not produce a nil interface: it produces a non-nil interface wrapping a nil
// pointer. Every `h.deps.X == nil` guard then reads false and the first method
// call dereferences the nil.
//
// The wiring cannot be trusted to get this right field by field, because the
// mistake is invisible at the call site — `SemanticReader: deps.Semantic.Reader`
// looks identical whether or not the source is nil. The contract belongs to
// this package, which is where the nil-means-absent guards are written, so this
// package makes the representation match the meaning for every field at once,
// including fields added later.
//
// The L2 semantic seams are why it exists: InitSemantic leaves Reader and
// Writer nil whenever Redis is reached through Sentinel or Cluster, and with
// the cache enabled every L1 miss went through the defeated guard and panicked.
func unboxNilDeps(d *Deps) []string {
	if d == nil {
		return nil
	}
	rv := reflect.ValueOf(d).Elem()
	rt := rv.Type()
	var fixed []string
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := rv.Field(i)
		if fv.Kind() != reflect.Interface || fv.IsNil() {
			continue
		}
		if isTypedNil(fv.Elem()) {
			fv.Set(reflect.Zero(f.Type))
			fixed = append(fixed, f.Name)
		}
	}
	return fixed
}

// isTypedNil reports whether a concrete value inside an interface is itself
// nil. Only the kinds that can be nil are considered; a zero struct is a
// legitimate value, not an absent dependency.
func isTypedNil(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

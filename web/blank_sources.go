package web

import "reflect"

// blankResultPaths returns a copy of r with the named Go field paths zeroed —
// the fields the framework read ONLY to feed a selected computed field and
// that the consumer did not ask for (queryschema.UnrequestedComputedSources).
//
// Blanking happens on the RESULT, after the Query's FromQueryResult already
// derived the computed value from those sources, and before the Response
// projection runs. A sparse Response then elides the blanked field exactly as
// it elides any unselected one, so `?fields=<computed>` answers with the
// computed field alone even when a source shares the wire shape.
//
// Nested paths ("Addresses.City") descend through structs and slice elements,
// blanking the leaf on every element. A path that does not resolve is skipped:
// the boot guard already proved the sources exist, so a miss here can only be
// a shape the walker does not model, and silently leaving the value alone is
// safer than panicking on a read.
func blankResultPaths[TResult any](r TResult, paths []string) TResult {
	if len(paths) == 0 {
		return r
	}
	v := reflect.ValueOf(&r).Elem()
	for _, p := range paths {
		blankPath(v, splitGoPath(p))
	}
	return r
}

// splitGoPath cuts a dotted Go field path into its segments.
func splitGoPath(p string) []string {
	segs := make([]string, 0, 2)
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '.' {
			segs = append(segs, p[start:i])
			start = i + 1
		}
	}
	return append(segs, p[start:])
}

// blankPath walks segs from v and zeroes the leaf. Pointers are followed (a
// nil pointer means there is nothing to blank); a slice fans out to every
// element so a segment leaf is blanked on all of them.
func blankPath(v reflect.Value, segs []string) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			blankPath(v.Index(i), segs)
		}
		return
	}
	if v.Kind() != reflect.Struct || len(segs) == 0 {
		return
	}
	f := v.FieldByName(segs[0])
	if !f.IsValid() {
		return
	}
	if len(segs) == 1 {
		if f.CanSet() {
			f.Set(reflect.Zero(f.Type()))
		}
		return
	}
	blankPath(f, segs[1:])
}

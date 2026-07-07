package web

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// pathFieldKind enumerates the supported Go types behind a `path:"<name>"`
// struct tag. The wrapper builds a per-Request plan once at construction time
// and the per-request binding walks that plan with no further reflection
// beyond the field SetXxx call.
type pathFieldKind int

const (
	pathKindString pathFieldKind = iota + 1
	pathKindInt
	pathKindUint
	pathKindFloat
	pathKindBool
	pathKindUUID
	pathKindDomainID
)

// pathFieldPlan describes a single bound field — index inside the Request
// struct, the URL segment name read via c.Params, the conversion kind, and
// the bit-width when it matters (int/uint/float). Cached per reflect.Type
// via pathSchemaCache so we never re-inspect the same Request twice.
type pathFieldPlan struct {
	fieldIndex int
	segment    string
	jsonName   string // first segment of json tag if present (for the dual-tag conflict check)
	kind       pathFieldKind
	bits       int
}

// pathSchema is the cached inspection result for a Request type — the list
// of path-bound fields plus a fast lookup of segment names declared (used by
// the boot checks of §4.1).
type pathSchema struct {
	fields    []pathFieldPlan
	bySegment map[string]int // segment name -> index in fields
}

// pathSchemaCache memoizes pathSchema by reflect.Type. Lives separate from
// schemaCache (query/filter allowlist) and expectedKeysCache (FullBody)
// because each carries a distinct shape.
var pathSchemaCache sync.Map // map[reflect.Type]*pathSchema

// inspectPathTags walks the exported fields of a Request type, picks the
// ones tagged `path:"<name>"`, validates type/shape, and caches the result.
// Panics on any structural violation (pointer field, slice/struct field,
// unknown type, conflicting json tag) — those are wrapper-construction
// errors that abort boot by design (§7.2).
func inspectPathTags(t reflect.Type) *pathSchema {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if cached, ok := pathSchemaCache.Load(t); ok {
		return cached.(*pathSchema)
	}
	schema := &pathSchema{bySegment: map[string]int{}}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		segment := f.Tag.Get("path")
		if segment == "" {
			continue
		}
		if f.Anonymous {
			panic(formatPathBootError(t, f, "anonymous embedded field cannot carry a path: tag"))
		}
		if !f.IsExported() {
			panic(formatPathBootError(t, f, "unexported field cannot carry a path: tag"))
		}
		if jsonTag := f.Tag.Get("json"); jsonTag != "" && jsonTag != "-" {
			panic(formatPathBootError(t, f, "field declares both path:\""+segment+"\" and json:\""+jsonTag+"\" — same value cannot come from two sources"))
		}
		kind, bits, err := classifyPathFieldType(f.Type)
		if err != "" {
			panic(formatPathBootError(t, f, err))
		}
		if _, dup := schema.bySegment[segment]; dup {
			panic(formatPathBootError(t, f, "segment :"+segment+" already bound to another field on the same Request"))
		}
		jsonName := ""
		schema.bySegment[segment] = len(schema.fields)
		schema.fields = append(schema.fields, pathFieldPlan{
			fieldIndex: i,
			segment:    segment,
			jsonName:   jsonName,
			kind:       kind,
			bits:       bits,
		})
	}
	pathSchemaCache.Store(t, schema)
	return schema
}

// classifyPathFieldType maps a Go reflect.Type to a pathFieldKind. Returns
// an error message string on rejection (empty string = ok). Pointer/slice/
// struct rejection is intentional — URL segments are always present and
// scalar (§6).
func classifyPathFieldType(ft reflect.Type) (pathFieldKind, int, string) {
	// domain.ID and uuid.UUID are concrete struct types — check by identity
	// before the generic struct rejection below.
	if ft == reflect.TypeOf(domain.ID{}) {
		return pathKindDomainID, 0, ""
	}
	if ft == reflect.TypeOf(uuid.UUID{}) {
		return pathKindUUID, 0, ""
	}
	switch ft.Kind() {
	case reflect.String:
		return pathKindString, 0, ""
	case reflect.Int:
		return pathKindInt, strconv.IntSize, ""
	case reflect.Int8:
		return pathKindInt, 8, ""
	case reflect.Int16:
		return pathKindInt, 16, ""
	case reflect.Int32:
		return pathKindInt, 32, ""
	case reflect.Int64:
		return pathKindInt, 64, ""
	case reflect.Uint:
		return pathKindUint, strconv.IntSize, ""
	case reflect.Uint8:
		return pathKindUint, 8, ""
	case reflect.Uint16:
		return pathKindUint, 16, ""
	case reflect.Uint32:
		return pathKindUint, 32, ""
	case reflect.Uint64:
		return pathKindUint, 64, ""
	case reflect.Float32:
		return pathKindFloat, 32, ""
	case reflect.Float64:
		return pathKindFloat, 64, ""
	case reflect.Bool:
		return pathKindBool, 0, ""
	case reflect.Pointer:
		return 0, 0, "path: tag must be on a non-pointer field; URL segments are always present"
	case reflect.Slice, reflect.Array:
		return 0, 0, "path: tag does not support slice/array types; segments are scalar"
	case reflect.Struct:
		return 0, 0, "path: tag does not support custom struct types (only domain.ID and uuid.UUID — see §6)"
	}
	return 0, 0, "path: tag does not support field type " + ft.String()
}

// applyPathBinding walks a pre-built schema and sets each field on req from
// c.Params. Returns (badField, true) on conversion failure (caller forwards
// to RespondSchemaViolation); returns ("", true) on success or empty schema.
// req must be addressable — wrappers pass &req via reflect.ValueOf which
// the caller already prepared.
func applyPathBinding(c fiber.Ctx, schema *pathSchema, reqVal reflect.Value) (string, bool) {
	if schema == nil || len(schema.fields) == 0 {
		return "", true
	}
	if reqVal.Kind() == reflect.Pointer {
		reqVal = reqVal.Elem()
	}
	for _, plan := range schema.fields {
		raw := c.Params(plan.segment)
		fv := reqVal.Field(plan.fieldIndex)
		if err := setPathField(fv, plan, raw); err {
			return plan.segment, false
		}
	}
	return "", true
}

// setPathField runs the conversion + assignment for one field. Returns true
// on conversion failure.
func setPathField(fv reflect.Value, plan pathFieldPlan, raw string) bool {
	switch plan.kind {
	case pathKindString:
		fv.SetString(raw)
	case pathKindInt:
		n, err := strconv.ParseInt(raw, 10, plan.bits)
		if err != nil {
			return true
		}
		fv.SetInt(n)
	case pathKindUint:
		n, err := strconv.ParseUint(raw, 10, plan.bits)
		if err != nil {
			return true
		}
		fv.SetUint(n)
	case pathKindFloat:
		f, err := strconv.ParseFloat(raw, plan.bits)
		if err != nil {
			return true
		}
		fv.SetFloat(f)
	case pathKindBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return true
		}
		fv.SetBool(b)
	case pathKindUUID:
		u, err := uuid.Parse(raw)
		if err != nil {
			return true
		}
		fv.Set(reflect.ValueOf(u))
	case pathKindDomainID:
		if _, err := uuid.Parse(raw); err != nil {
			return true
		}
		fv.Set(reflect.ValueOf(domain.NewID(raw)))
	default:
		return true
	}
	return false
}

// hasPathSegment reports whether a Request type declares a `path:"<name>"`
// tag matching segmentName. Used by handle_command_with_body / handle_query
// to enforce the §4.1 conflict (no `path:"id"` on :id-binding wrappers).
func hasPathSegment(t reflect.Type, segmentName string) bool {
	schema := inspectPathTags(t)
	_, ok := schema.bySegment[segmentName]
	return ok
}

// hasAnyPathTag reports whether the Request declares at least one path: tag.
// Used by the boot WARNING of §4.2 — when a Group A wrapper is paired with
// an ID-requiring auto handler and the Request declares no path: tag at
// all, the framework cannot guarantee an ID source.
func hasAnyPathTag(t reflect.Type) bool {
	return len(inspectPathTags(t).fields) > 0
}

// formatPathBootError renders the standard boot diagnostic for path: tag
// structural violations. Carries the Request type, the offending field, and
// the human-readable reason.
func formatPathBootError(t reflect.Type, f reflect.StructField, reason string) string {
	return fmt.Sprintf(
		"\n[omnicore] FATAL: invalid path: tag\n\n  request: %s\n  field:   %s (%s)\n  reason:  %s\n",
		t.String(), f.Name, f.Type.String(), reason,
	)
}

// BindPath populates fields of req tagged `path:"<name>"` from the matching
// Fiber URL segment (c.Params("<name>")). Used by manual fiber.Handler
// closures that opt out of CommandWith*/QueryWith* but still
// want the declarative binding the wrappers do automatically.
//
// On the first type-conversion failure returns (badField, false). Callers
// forward badField to RespondSchemaViolation to emit the canonical 400
// envelope — same shape the wrapper produces. req must be a pointer to a
// struct; returns ("", true) when the struct has no `path:` tags.
//
// Same cached pathSchema the wrapper uses; mirrors fwweb.ParseCriteria and
// fwweb.RespondPaged — manual handlers chain BindPath → ParseCriteria →
// ToCommand/ToQuery → Dispatch.
func BindPath(c fiber.Ctx, req any) (string, bool) {
	if req == nil {
		return "", true
	}
	v := reflect.ValueOf(req)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		panic("fwweb.BindPath: req must be a non-nil pointer to a struct")
	}
	if v.Elem().Kind() != reflect.Struct {
		panic("fwweb.BindPath: req must be a pointer to a struct")
	}
	schema := inspectPathTags(v.Elem().Type())
	return applyPathBinding(c, schema, v)
}

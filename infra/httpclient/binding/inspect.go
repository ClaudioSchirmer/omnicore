package binding

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
)

// ioReaderType is the reflect.Type of io.Reader, computed once so the
// per-field validation does not allocate a fresh type descriptor each
// time. bindBodyStream fields must implement this interface.
var ioReaderType = reflect.TypeOf((*io.Reader)(nil)).Elem()

// multipartType is the reflect.Type of binding.Multipart, computed once
// for the bindBodyMultipart field-type validation.
var multipartType = reflect.TypeOf(Multipart{})

// fieldBinding is the inspector's per-field record. The kind selects which
// part of the request/response carries the value; name is the wire-format
// identifier (URL placeholder, query key, header name); codec is the body
// encoder name for bindBody.
type fieldBinding struct {
	fieldIndex []int
	kind       bindKind
	name       string
	codec      string

	// goType describes the field's static Go type — recorded once at
	// inspection to keep BuildRequest off the reflect type discovery path.
	goType reflect.Type
}

// typePlan is the cached binding plan for one Go struct type. Inspectors
// compute it once per (reflect.Type, role) pair and BuildRequest /
// DecodeResponse consume it on every call.
//
// hasBody / bodyAt let request assembly take the body fast path without
// re-scanning the bindings slice; the same record points at the body field
// on the response side when a "body,..." tag appears on Resp.
type typePlan struct {
	bindings []fieldBinding
	hasBody  bool
	bodyAt   int // index into bindings when hasBody, -1 otherwise

	// hasStreamingBody reports whether the request body comes from an
	// io.Reader (bindBodyStream) or an httpclient.Multipart streamed via
	// a pipe (bindBodyMultipart). The Call surface reads this flag to set
	// obs.streamingRequest before chain dispatch — that disables retry
	// replay and body buffering in the logging middleware.
	hasStreamingBody bool
}

// inspectRole distinguishes request inspection (validates path placeholders
// against tagged fields) from response inspection (no path semantics — only
// body/header tags are meaningful). The same Go type can serve both roles in
// theory; the cache keys on (type, role) so the validations don't collide.
type inspectRole int

const (
	roleRequest  inspectRole = 1
	roleResponse inspectRole = 2
)

type planKey struct {
	t    reflect.Type
	role inspectRole
}

var planCache sync.Map // map[planKey]*planEntry

// planEntry holds the cached result and any inspection error so subsequent
// callers see the same outcome without re-inspecting.
type planEntry struct {
	plan *typePlan
	err  error
}

// pathPlaceholderRE matches "{name}" path placeholders. The name follows the
// same rules as Go identifiers (letters, digits, underscore) since YAML
// authors typically mirror the request struct field name.
var pathPlaceholderRE = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_-]*)\}`)

// inspectRequestType returns the binding plan for a request Go type. Path is
// the URL template used to validate that every {placeholder} has a matching
// http:"path,name" field, and vice-versa.
func inspectRequestType(t reflect.Type, path string) (*typePlan, error) {
	return inspectType(t, roleRequest, path)
}

// HasStreamingBody reports whether the request type carries a
// http:"body,stream" or http:"body,multipart" tag. The Call surface
// uses this to set obs.streamingRequest before chain dispatch, which
// disables retry replay and request body buffering in the logging
// middleware. Returns false on any inspection error so the actual error
// surfaces at BuildRequest time with the operator-actionable message.
func HasStreamingBody(reqType any, path string) bool {
	if reqType == nil {
		return false
	}
	rv := reflect.ValueOf(reqType)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			// Typed-nil pointer (var p *Req = nil) — reach the struct zero
			// value of the element type so the inspector can still walk the
			// fields. reflect.New(T) returns *T; the extra .Elem() unwraps
			// it to T so the loop falls through the Kind==Struct guard
			// below.
			rv = reflect.New(rv.Type().Elem()).Elem()
			break
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return false
	}
	plan, err := inspectRequestType(rv.Type(), path)
	if err != nil {
		return false
	}
	return plan.hasStreamingBody
}

// inspectResponseType returns the binding plan for a response Go type. Path
// is ignored on the response side.
func inspectResponseType(t reflect.Type) (*typePlan, error) {
	return inspectType(t, roleResponse, "")
}

func inspectType(t reflect.Type, role inspectRole, path string) (*typePlan, error) {
	if t == nil {
		return nil, fmt.Errorf("nil type")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("binding: expected struct, got %s", t.Kind())
	}
	key := planKey{t: t, role: role}
	if cached, ok := planCache.Load(key); ok {
		entry := cached.(*planEntry)
		return entry.plan, entry.err
	}
	plan, err := buildPlan(t, role, path)
	planCache.Store(key, &planEntry{plan: plan, err: err})
	return plan, err
}

func buildPlan(t reflect.Type, role inspectRole, path string) (*typePlan, error) {
	plan := &typePlan{bodyAt: -1}
	seenBody := false
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("http")
		b, present, err := parseHTTPTag(tag)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		if !present {
			continue
		}
		b.fieldIndex = f.Index
		b.goType = f.Type
		if err := validateFieldType(t.Name(), f.Name, b); err != nil {
			return nil, err
		}
		if b.kind == bindBody || b.kind == bindBodyStream || b.kind == bindBodyMultipart {
			if seenBody {
				return nil, fmt.Errorf("%s: more than one http:\"body,...\" field is not supported", t.Name())
			}
			seenBody = true
			plan.hasBody = true
			plan.bodyAt = len(plan.bindings)
			if b.kind != bindBody {
				plan.hasStreamingBody = true
			}
		}
		plan.bindings = append(plan.bindings, b)
	}
	if role == roleRequest {
		if err := validatePathCoverage(t.Name(), path, plan.bindings); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// validateFieldType rejects field kinds the binding layer cannot serialize.
// Path / query single / header expect scalar string-convertible types.
// query,csv and query,multi expect slices or arrays. headersMap expects
// map[string]string. body is open.
func validateFieldType(structName, fieldName string, b fieldBinding) error {
	t := b.goType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch b.kind {
	case bindPath, bindQuerySingle, bindHeader:
		if !scalarLikeKind(t.Kind()) {
			return fmt.Errorf("%s.%s: tag http:%q expects a scalar (string/int/etc.), got %s", structName, fieldName, kindLabel(b), t.Kind())
		}
	case bindQueryCSV, bindQueryMulti:
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return fmt.Errorf("%s.%s: tag http:%q expects a slice or array, got %s", structName, fieldName, kindLabel(b), t.Kind())
		}
	case bindHeadersMap:
		if t.Kind() != reflect.Map || t.Key().Kind() != reflect.String || t.Elem().Kind() != reflect.String {
			return fmt.Errorf("%s.%s: tag http:\"headers\" expects map[string]string, got %s", structName, fieldName, t)
		}
	case bindBody:
		// open — codec validates at encode/decode time
	case bindBodyStream:
		if !b.goType.Implements(ioReaderType) {
			return fmt.Errorf("%s.%s: tag http:\"body,stream\" expects an io.Reader, got %s", structName, fieldName, b.goType)
		}
	case bindBodyMultipart:
		if b.goType != multipartType {
			return fmt.Errorf("%s.%s: tag http:\"body,multipart\" expects an httpclient.Multipart, got %s", structName, fieldName, b.goType)
		}
	}
	return nil
}

func validatePathCoverage(structName, path string, bindings []fieldBinding) error {
	if path == "" {
		return nil
	}
	templated := map[string]struct{}{}
	for _, m := range pathPlaceholderRE.FindAllStringSubmatch(path, -1) {
		templated[m[1]] = struct{}{}
	}
	tagged := map[string]struct{}{}
	for _, b := range bindings {
		if b.kind == bindPath {
			tagged[b.name] = struct{}{}
		}
	}
	var missing []string
	for name := range templated {
		if _, ok := tagged[name]; !ok {
			missing = append(missing, name)
		}
	}
	var orphan []string
	for name := range tagged {
		if _, ok := templated[name]; !ok {
			orphan = append(orphan, name)
		}
	}
	if len(missing) > 0 || len(orphan) > 0 {
		var msgs []string
		if len(missing) > 0 {
			msgs = append(msgs, fmt.Sprintf("path placeholder(s) %v have no matching http:\"path,name\" field", missing))
		}
		if len(orphan) > 0 {
			msgs = append(msgs, fmt.Sprintf("http:\"path,name\" tag(s) %v do not appear in the path %q", orphan, path))
		}
		return fmt.Errorf("%s: %s", structName, strings.Join(msgs, "; "))
	}
	return nil
}

func scalarLikeKind(k reflect.Kind) bool {
	switch k {
	case reflect.String,
		reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func kindLabel(b fieldBinding) string {
	switch b.kind {
	case bindPath:
		return "path," + b.name
	case bindQuerySingle:
		return "query," + b.name
	case bindQueryCSV:
		return "query," + b.name + ",csv"
	case bindQueryMulti:
		return "query," + b.name + ",multi"
	case bindHeader:
		return "header," + b.name
	case bindHeadersMap:
		return "headers"
	case bindBody:
		return "body," + b.codec
	}
	return ""
}

// resetPlanCache is used by tests to clear cached plans between subtests
// that intentionally redefine a type's bindings.
func resetPlanCache() {
	planCache = sync.Map{}
}

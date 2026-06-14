package domain

import (
	"fmt"
	"reflect"
	"sync"
)

// TranslationVarsProvider is the optional escape-hatch interface a Notification
// implements when its translation variables cannot be expressed as `tvar` tags
// on exported fields (e.g. the value lives on an unexported field, is derived
// from multiple inputs, or is shaped by ctx-aware logic). When present, the
// method's return value REPLACES (does not merge with) the tag-based
// extraction — declaring both a TranslationVars method and `tvar` fields is
// supported and the method wins.
type TranslationVarsProvider interface {
	TranslationVars() map[string]string
}

// varField holds a single (placeholder name → struct field path) entry on the
// reflection plan. index is the field index path used by reflect.Value.FieldByIndex,
// supporting fields nested inside anonymous embeds.
type varField struct {
	index []int
	name  string
}

// varPlanCache memoizes the per-type reflection plan so the second emission of
// a notification type is a constant-time map lookup + a few direct field reads.
var varPlanCache sync.Map // map[reflect.Type][]varField

// ExtractVarsFromTags returns the translation-variable map a Notification
// contributes through its `tvar`-tagged fields (or via the
// TranslationVarsProvider escape hatch).
//
// Resolution order:
//
//  1. nil input → nil output.
//  2. n implements TranslationVarsProvider → return the method's result
//     verbatim. Tags are NOT walked in this case (deliberate; the escape
//     hatch is opt-in for full control).
//  3. Otherwise reflect over the dynamic struct: each exported field carrying
//     a non-empty `tvar:"name"` (and not `tvar:"-"`) contributes one entry,
//     value rendered via fmt.Sprint. Pointers are dereferenced; nil pointers
//     contribute an empty string.
//
// Returns nil when no entry was produced — callers treat nil and empty
// identically so the hot path of vars-less notifications stays allocation-free.
func ExtractVarsFromTags(n Notification) map[string]string {
	if n == nil {
		return nil
	}
	if p, ok := n.(TranslationVarsProvider); ok {
		return p.TranslationVars()
	}
	t := reflect.TypeOf(n)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	plan := loadVarPlan(t)
	if len(plan) == 0 {
		return nil
	}
	v := reflect.ValueOf(n)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	out := make(map[string]string, len(plan))
	for _, f := range plan {
		fv := v.FieldByIndex(f.index)
		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				out[f.name] = ""
				continue
			}
			fv = fv.Elem()
		}
		out[f.name] = fmt.Sprint(fv.Interface())
	}
	return out
}

// MessageVars merges a NotificationMessage's tag-derived vars (from
// ExtractVarsFromTags) with its per-emit Vars override (call-site values win
// on key collision). Returns nil when both sources are empty so transport
// layers can short-circuit allocation on the common no-vars path.
func MessageVars(msg NotificationMessage) map[string]string {
	notifVars := ExtractVarsFromTags(msg.Notification)
	if len(notifVars) == 0 && len(msg.Vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(notifVars)+len(msg.Vars))
	for k, v := range notifVars {
		out[k] = v
	}
	for k, v := range msg.Vars {
		out[k] = v
	}
	return out
}

func loadVarPlan(t reflect.Type) []varField {
	if cached, ok := varPlanCache.Load(t); ok {
		return cached.([]varField)
	}
	plan := buildVarPlan(t, nil)
	varPlanCache.Store(t, plan)
	return plan
}

func buildVarPlan(t reflect.Type, parent []int) []varField {
	var out []varField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		idx := make([]int, 0, len(parent)+1)
		idx = append(idx, parent...)
		idx = append(idx, i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, buildVarPlan(f.Type, idx)...)
			continue
		}
		tag, ok := f.Tag.Lookup("tvar")
		if !ok || tag == "" || tag == "-" {
			continue
		}
		out = append(out, varField{index: idx, name: tag})
	}
	return out
}

package requests

import (
	"reflect"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/internal/fieldcopy"
)

// The Request→Command builder: one copier compiled per type pair, cached, and
// no serialization anywhere. The REQUEST drives the walk — every one of its
// fields must land on a same-named Command field — while Command fields the
// Request does not supply stay zero, to be filled by the path id, an identity
// overlay or the handler itself.

type pairKey struct{ req, cmd reflect.Type }

type pairEntry struct {
	copier fieldcopy.Copier
	reason string
}

var pairCache sync.Map // pairKey → *pairEntry

// AutoFromRequest builds TCmd from a Request that embeds [Auto].
//
// TCmd is written explicitly at the call site and the Request type is inferred
// from the argument, so the call reads as the seat it implements:
//
//	return fwrequests.AutoFromRequest[*commands.InsertUserCommand](r)
//
// A pointer TCmd is allocated here (the pipeline operates over Commands as
// pointers). The pair is validated at boot by the command constructors; if an
// unvalidated bad pair ever reaches this function — a hand-rolled call site, a
// test — it fails loudly with the same diagnostic, because a Request that
// embedded [Auto] declared that this travel works.
func AutoFromRequest[TCmd any, TReq AutoMapper](req TReq) TCmd {
	var cmd TCmd
	cmdType := reflect.TypeOf(cmd)

	out := reflect.ValueOf(&cmd).Elem()
	target := out
	if cmdType != nil && cmdType.Kind() == reflect.Pointer {
		alloc := reflect.New(cmdType.Elem())
		out.Set(alloc)
		target = alloc.Elem()
	}

	reqV := reflect.ValueOf(req)
	for reqV.Kind() == reflect.Pointer {
		if reqV.IsNil() {
			return cmd // nothing to read; the zero Command stands
		}
		reqV = reqV.Elem()
	}

	entry := pairEntryFor(reqV.Type(), target.Type())
	if entry.copier == nil {
		panic(FormatAutoRequestGuard(reqV.Type(), cmdType, entry.reason))
	}
	entry.copier(reqV, target)
	return cmd
}

// AutoRequestReason reports why a Request cannot build a Command by
// assignment, or "" when it can. It is the diagnostic seat the command
// constructors consult at Mount — and calling it there pre-warms the cache.
//
// Only the fields the REQUEST declares are examined, so a Command may carry
// anything beyond them.
func AutoRequestReason(reqType, cmdType reflect.Type) string {
	req, reqOK := derefStruct(reqType)
	cmd, cmdOK := derefStruct(cmdType)
	if !reqOK {
		return "the Request type " + typeLabel(reqType) + " is not a struct"
	}
	if !cmdOK {
		return "the Command type " + typeLabel(cmdType) + " is not a struct"
	}
	return pairEntryFor(req, cmd).reason
}

func derefStruct(t reflect.Type) (reflect.Type, bool) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil, false
	}
	return t, true
}

func pairEntryFor(reqT, cmdT reflect.Type) *pairEntry {
	key := pairKey{reqT, cmdT}
	if v, ok := pairCache.Load(key); ok {
		return v.(*pairEntry)
	}
	copier, reason := buildCommandCopier(reqT, cmdT, map[pairKey]bool{})
	entry := &pairEntry{reason: reason}
	if reason == "" {
		entry.copier = copier
	}
	pairCache.Store(key, entry)
	return entry
}

// buildCommandCopier compiles the copier for one (Request, Command) struct
// pair, or reports why it cannot. The REQUEST drives: every one of its
// exported fields must find a same-named Command field to write into.
func buildCommandCopier(reqT, cmdT reflect.Type, inProgress map[pairKey]bool) (fieldcopy.Copier, string) {
	key := pairKey{reqT, cmdT}
	if inProgress[key] {
		return func(src, dst reflect.Value) {
			if e := pairEntryFor(reqT, cmdT); e.copier != nil {
				e.copier(src, dst)
			}
		}, ""
	}
	inProgress[key] = true
	defer delete(inProgress, key)

	// The Request is the wire side, so `json:"-"` marks a field that never
	// travels — it is not expected to reach the Command either.
	reqFields := fieldcopy.ExportedFields(reqT, true)
	cmdFields := fieldcopy.ExportedFields(cmdT, false)

	type slot struct {
		src, dst []int
		copy     fieldcopy.Copier
	}
	var slots []slot
	nested := func(s, d reflect.Type) (fieldcopy.Copier, string) {
		return buildCommandCopier(s, d, inProgress)
	}

	for name, reqIdx := range reqFields {
		cmdIdx, ok := cmdFields[name]
		if !ok {
			return nil, "field " + name + " has no same-named field on " + cmdT.String() +
				" — a wire value with nowhere to land"
		}
		sf := reqT.FieldByIndex(reqIdx)
		df := cmdT.FieldByIndex(cmdIdx)
		cp, reason := fieldcopy.ValueCopier(sf.Type, df.Type, nested)
		if reason != "" {
			return nil, "field " + name + " (" + sf.Type.String() + " → " + df.Type.String() + "): " + reason
		}
		slots = append(slots, slot{src: reqIdx, dst: cmdIdx, copy: cp})
	}

	return func(srcV, dstV reflect.Value) {
		for i := range slots {
			s := &slots[i]
			sv, err := srcV.FieldByIndexErr(s.src)
			if err != nil || !sv.IsValid() {
				continue
			}
			dv := fieldcopy.FieldAlloc(dstV, s.dst)
			if !dv.IsValid() || !dv.CanSet() {
				continue
			}
			s.copy(sv, dv)
		}
	}, ""
}

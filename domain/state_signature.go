package domain

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"
)

// The state signature is what makes "the domain has the last word" checkable
// rather than merely intended.
//
// A ValidEntity does not hold a copy of the entity — it holds a POINTER, and the
// write path reads the business values from the live object at write time. So
// the seal proves PROVENANCE (only this package can produce one, and only after
// validation) but, on its own, proves nothing about the VALUES: anything holding
// the entity between the Get* and the write could change a field and have it
// persisted as though the domain had decided it.
//
// The signature closes that. The Get* hashes the entity onto the sealed value;
// the write path recomputes and compares before writing. Same state, same
// number. Different number, someone wrote to the entity after the domain was
// done, and the write is refused.
//
// This is an INTEGRITY check, not a cryptographic signature. Signer and verifier
// are the same process with no secret between them, so an HMAC would add
// ceremony without adding protection — the seal is what keeps a forged value
// out. What this catches is the adversary that actually exists in one process: a
// mistake.
//
// SCOPE. The walk visits EXPORTED fields only, which on a framework entity means
// exactly the business fields the developer declared: identity, revision,
// timestamps, notifications, mode and the aggregate's child map all live in
// unexported carriers (Managed, BaseEntity, AggregateRoot) and are skipped by
// construction. The id being out of the comparison matters — infra stamps the
// minted id back onto the entity after an insert, and the check must not read
// that as tampering. Aggregate children are walked explicitly, since the same
// unexported-field rule would otherwise hide them.
//
// It also covers fields the TableSchema does not persist. That is deliberate:
// the rule is "after the seal, do not touch the entity", not "do not touch the
// columns".

// stateSeed is fixed for the process, so the two computations of one write
// agree. It is never persisted, logged, or compared across processes.
var stateSeed = maphash.MakeSeed()

// fieldPlanCache memoizes the exported-field index list per struct type, so the
// walk never re-derives type metadata — the same move TableSchema makes for its
// column plan.
var fieldPlanCache sync.Map // reflect.Type -> []int

var timeType = reflect.TypeOf(time.Time{})

func fieldPlan(t reflect.Type) []int {
	if p, ok := fieldPlanCache.Load(t); ok {
		return p.([]int)
	}
	idx := make([]int, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			idx = append(idx, i)
		}
	}
	fieldPlanCache.Store(t, idx)
	return idx
}

// stateSignature hashes the entity's business state plus its aggregate
// children. A nil entity answers 0, which is also the zero value of the sealed
// field — so a ValidEntity that never came from a Get* is recognizable.
func stateSignature(e Entity) uint64 {
	if e == nil {
		return 0
	}
	var h maphash.Hash
	h.SetSeed(stateSeed)
	var scratch [8]byte
	hashValue(&h, reflect.ValueOf(e), scratch[:])
	hashChildren(&h, e, scratch[:])
	return h.Sum64()
}

// hashChildren folds the aggregate's collections in. The child map's iteration
// order is random in Go, so the type names are SORTED before walking — without
// that, identical state would hash differently from one call to the next and
// every write would look tampered with. Within a type the order is preserved: it
// is the order the persister writes in, so it is part of the state.
//
// Each item contributes its statuses as well as its fields, because the pair
// (original, current) is what decides whether the persister inserts, updates,
// archives or skips it — a child flipped to Removed changes the write without
// changing a single value.
func hashChildren(h *maphash.Hash, e Entity, scratch []byte) {
	prov, ok := e.(AggregateRootProvider)
	if !ok {
		return
	}
	root := prov.GetAggregateRoot()
	if root == nil || len(root.aggregates) == 0 {
		return
	}
	names := make([]string, 0, len(root.aggregates))
	for name := range root.aggregates {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		_, _ = h.WriteString(name)
		list := root.aggregates[name]
		binary.LittleEndian.PutUint64(scratch, uint64(len(list)))
		_, _ = h.Write(scratch)
		for _, entry := range list {
			binary.LittleEndian.PutUint64(scratch, uint64(entry.originalStatus))
			_, _ = h.Write(scratch)
			binary.LittleEndian.PutUint64(scratch, uint64(entry.currentStatus))
			_, _ = h.Write(scratch)
			hashValue(h, reflect.ValueOf(entry.item), scratch)
		}
	}
}

// hashValue writes v's canonical form into h. The encoding is deliberate, not
// incidental: field order follows the struct's declaration order, a nil pointer
// is distinguishable from a zero value by its marker byte, a slice writes its
// length before its elements (so [a] and [a,a] differ), and map keys are sorted
// (Go's map order is random, and an unsorted walk would produce a different
// number for identical state).
func hashValue(h *maphash.Hash, v reflect.Value, scratch []byte) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			_ = h.WriteByte(0)
			return
		}
		_ = h.WriteByte(1)
		hashValue(h, v.Elem(), scratch)

	case reflect.String:
		// WriteString avoids the []byte conversion a plain hash.Hash forces,
		// which would be one allocation per text field.
		_, _ = h.WriteString(v.String())

	case reflect.Bool:
		if v.Bool() {
			_ = h.WriteByte(1)
		} else {
			_ = h.WriteByte(0)
		}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		binary.LittleEndian.PutUint64(scratch, uint64(v.Int()))
		_, _ = h.Write(scratch)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		binary.LittleEndian.PutUint64(scratch, v.Uint())
		_, _ = h.Write(scratch)

	case reflect.Float32, reflect.Float64:
		// The bit pattern, not a scaled integer: a rounded encoding would let
		// two different stored values hash the same.
		binary.LittleEndian.PutUint64(scratch, math.Float64bits(v.Float()))
		_, _ = h.Write(scratch)

	case reflect.Slice, reflect.Array:
		binary.LittleEndian.PutUint64(scratch, uint64(v.Len()))
		_, _ = h.Write(scratch)
		for i := 0; i < v.Len(); i++ {
			hashValue(h, v.Index(i), scratch)
		}

	case reflect.Map:
		binary.LittleEndian.PutUint64(scratch, uint64(v.Len()))
		_, _ = h.Write(scratch)
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return mapKeyLess(keys[i], keys[j]) })
		for _, k := range keys {
			hashValue(h, k, scratch)
			hashValue(h, v.MapIndex(k), scratch)
		}

	case reflect.Struct:
		if v.Type() == timeType {
			// Box ONLY a real time value: the type check keeps this off every
			// other struct, which is where a naive walk spends its allocations.
			binary.LittleEndian.PutUint64(scratch, uint64(v.Interface().(time.Time).UnixNano()))
			_, _ = h.Write(scratch)
			return
		}
		for _, i := range fieldPlan(v.Type()) {
			hashValue(h, v.Field(i), scratch)
		}
	}
}

// mapKeyLess orders map keys deterministically. Every kind must be covered:
// a kind that falls through to a constant answer leaves sort.Slice with the
// map's own random order, so identical state would hash differently between
// calls and writes would be refused at random. The fallback formats the value
// rather than giving up — it allocates, but only for key kinds no schema
// actually persists.
func mapKeyLess(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.String:
		return a.String() < b.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() < b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return a.Uint() < b.Uint()
	case reflect.Float32, reflect.Float64:
		return a.Float() < b.Float()
	case reflect.Bool:
		return !a.Bool() && b.Bool()
	default:
		return fmt.Sprint(a.Interface()) < fmt.Sprint(b.Interface())
	}
}

// verifyState recomputes the entity's signature and compares it with the one the
// seal recorded. The write path calls it BEFORE writing — the persister stamps
// minted child ids onto the aggregate mid-write, so a check placed after would
// read the framework's own bookkeeping as tampering.
//
// A nil source means the ValidEntity was not produced by a Get*. The sealed
// types are exported, so a zero value can be constructed from anywhere even
// though it can never be populated from outside this package; refusing it here
// is what keeps that empty shell from reaching the database.
func verifyState(source Entity, sealed uint64) error {
	if source == nil {
		return fmt.Errorf(
			"domain: this write shape carries no entity — it was not produced by a Get* function, " +
				"and only those can build one the write path will accept")
	}
	if got := stateSignature(source); got != sealed {
		return fmt.Errorf(
			"domain: %s was modified after the domain validated it — the write is refused because what "+
				"would be persisted is no longer what the rules approved. Everything between the Get* call "+
				"and the write must leave the entity alone; to change it again, validate again",
			classNameOf(source))
	}
	return nil
}

package queries

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// CursorSchemaVersion is the on-wire identifier of the cursor payload's shape.
// Bump when the tuple semantics change in a way readers cannot transparently
// honor. A cursor whose v field does not match this constant is rejected with
// ErrCursorInvalid; the wrapper turns that into 400 SchemaViolationNotification
// so the consumer surfaces the schema drift instead of silently navigating
// against an outdated cursor.
const CursorSchemaVersion = 1

// ErrCursorInvalid signals a cursor string that cannot be parsed under the
// current schema — malformed base64, malformed JSON, missing/unsupported
// version, or missing/empty K. The wrapper consumes it to emit 400
// SchemaViolationNotification. The reader uses the same error when the
// cursor reaches it with a malformed shape (defense in depth — the wrapper
// is expected to have validated already).
var ErrCursorInvalid = errors.New("cursor invalid")

// Cursor is the decoded payload of a paged-listing cursor. K is the tuple
// (sort_value_1, ..., sort_value_n, _id), where the trailing element is
// always the document's _id stringified for cross-call stability. When the
// request had no custom Sort, K degenerates to a single-element tuple
// containing only _id.
//
// H is the canonical SHA-256 of the issuing call's full listing context —
// see HashContext for the deterministic byte stream. Covers Filter, Sort,
// Search and IncludeArchived. Empty when the issuing call had the default
// context (no filter, no sort, no search, archived excluded). The reader
// compares H against the current call's context hash and rejects ANY
// mismatch — filter change, sort change, search change, archived flip —
// so a consumer that touches any listing axis mid-navigation is forced to
// request page 1 of the new context instead of silently navigating against
// a stale keyset boundary on a different result set.
//
// The reader builds the keyset filter (forward $gt / backward $lt cascade)
// from K, aligned positionally with ReadCriteria.Sort. The wrapper validates
// len(K)-1 == len(Sort) AND H matches HashContext(criteria...) after parsing
// the query string; either mismatch is rejected with 400
// SchemaViolationNotification on the field that carried the cursor.
type Cursor struct {
	K []any
	H string
}

// EncodeCursor serializes the tuple + filter hash as base64(URLEncoding) of
// the JSON object {"v":<CursorSchemaVersion>,"k":[...],"h":"<sha256>"}. The
// h field is omitted when filterHash is empty (the canonical hash of an
// empty filter). URL-safe encoding is used so cursors flow through query
// strings without `+` / `/` mangling. Returns the empty string + error only
// on json.Marshal failure (in practice unreachable for the value types the
// reader feeds in — strings, numbers, booleans).
func EncodeCursor(k []any, filterHash string) (string, error) {
	payload := struct {
		V int    `json:"v"`
		K []any  `json:"k"`
		H string `json:"h,omitempty"`
	}{V: CursorSchemaVersion, K: k, H: filterHash}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}

// DecodeCursor parses a cursor string into Cursor. Returns ErrCursorInvalid
// on every failure mode the wrapper needs to surface uniformly as 400 —
// malformed base64, malformed JSON, missing/unsupported v, missing/empty k.
// The h field (filter hash) is optional and decoded into Cursor.H; an absent
// h decodes as "" so the reader's filter-hash check stays uniform (empty
// filter hashes to "" too). Callers that need finer-grained diagnostics can
// compare against ErrCursorInvalid via errors.Is.
func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, ErrCursorInvalid
	}
	data, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrCursorInvalid
	}
	var payload struct {
		V int    `json:"v"`
		K []any  `json:"k"`
		H string `json:"h"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Cursor{}, ErrCursorInvalid
	}
	if payload.V != CursorSchemaVersion {
		return Cursor{}, ErrCursorInvalid
	}
	if len(payload.K) == 0 {
		return Cursor{}, ErrCursorInvalid
	}
	return Cursor{K: payload.K, H: payload.H}, nil
}

// HashContext returns the canonical SHA-256 hex of the full listing context
// that scopes a cursor — every input that shapes the underlying result set
// the cursor walks. Covers:
//
//   - filter: the Filter map (deterministic key sort at every depth)
//   - sortFields: declaration order + field + Desc flag per entry
//   - search: the raw `?search=` value (text-index query)
//   - includeArchived: the DeletedAt gate flag
//
// The empty / default context — no filter, no sort, no search, archived
// excluded — hashes to "" so cursors issued from the canonical first page
// carry no h field on the wire. Any non-default state hashes to a 64-char
// hex SHA-256.
//
// Used by the reader to validate cursor.H against the current call's full
// listing context. ANY mismatch (filter changed, sort changed, search
// changed, includeArchived flipped) is rejected with 400
// SchemaViolationNotification on the cursor's wire key so the consumer
// requests page 1 of the new context instead of silently navigating against
// a stale keyset boundary on a different result set.
//
// Symmetric to the sort tuple-length alignment check the wrapper performs
// in parallel — both checks must pass for a cursor to be honored.
func HashContext(filter map[string]any, sortFields []SortField, search string, includeArchived bool) string {
	if len(filter) == 0 && len(sortFields) == 0 && search == "" && !includeArchived {
		return ""
	}
	h := sha256.New()
	fmt.Fprint(h, "ctx_v1|filter:")
	canonicalizeFilterValue(h, filter)
	fmt.Fprintf(h, "|sort:%d", len(sortFields))
	for _, s := range sortFields {
		fmt.Fprintf(h, ":%d:%s:%t", len(s.Field), s.Field, s.Desc)
	}
	fmt.Fprintf(h, "|search:%d:%s", len(search), search)
	fmt.Fprintf(h, "|archived:%t", includeArchived)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalizeFilterValue writes a deterministic byte stream representing v
// into w. Recursive over maps/slices; tagged per type so distinct shapes can
// never collide via concatenation. The schema is internal to HashFilter and
// is NOT a serialization format — only the resulting SHA-256 leaks to the
// wire (as Cursor.H).
func canonicalizeFilterValue(w io.Writer, v any) {
	switch x := v.(type) {
	case nil:
		fmt.Fprint(w, "n")
	case bool:
		fmt.Fprintf(w, "b:%t", x)
	case string:
		fmt.Fprintf(w, "s:%d:%s", len(x), x)
	case int:
		fmt.Fprintf(w, "i:%d", int64(x))
	case int32:
		fmt.Fprintf(w, "i:%d", int64(x))
	case int64:
		fmt.Fprintf(w, "i:%d", x)
	case uint:
		fmt.Fprintf(w, "u:%d", uint64(x))
	case uint64:
		fmt.Fprintf(w, "u:%d", x)
	case float32:
		fmt.Fprintf(w, "f:%g", float64(x))
	case float64:
		fmt.Fprintf(w, "f:%g", x)
	case map[string]any:
		canonicalizeFilterMap(w, x)
	case []any:
		fmt.Fprintf(w, "a:%d[", len(x))
		for _, item := range x {
			canonicalizeFilterValue(w, item)
			fmt.Fprint(w, ",")
		}
		fmt.Fprint(w, "]")
	case []string:
		fmt.Fprintf(w, "ss:%d[", len(x))
		for _, item := range x {
			fmt.Fprintf(w, "%d:%s,", len(item), item)
		}
		fmt.Fprint(w, "]")
	case MultiClause:
		fmt.Fprint(w, "MC[")
		for _, c := range x.Clauses {
			canonicalizeFilterValue(w, c)
			fmt.Fprint(w, ",")
		}
		fmt.Fprint(w, "]")
	case Clause:
		fmt.Fprintf(w, "CL:%s:%d[", x.Op, len(x.Values))
		for _, val := range x.Values {
			canonicalizeFilterValue(w, val)
			fmt.Fprint(w, ",")
		}
		fmt.Fprint(w, "]")
	case TextMatch:
		fmt.Fprintf(w, "TM:%d:%t:%t:%d:%s", x.Kind, x.CaseInsensitive, x.Negate, len(x.Value), x.Value)
	case TextMatchList:
		fmt.Fprintf(w, "TML:%t:%t:%d[", x.CaseInsensitive, x.Negate, len(x.Values))
		for _, val := range x.Values {
			fmt.Fprintf(w, "%d:%s,", len(val), val)
		}
		fmt.Fprint(w, "]")
	default:
		// Unknown / unhandled value type — stringify with type tag so the
		// stream stays deterministic. Distinct types still produce distinct
		// hashes (the %T prefix encodes the Go type).
		fmt.Fprintf(w, "?%T:%v", v, v)
	}
}

func canonicalizeFilterMap(w io.Writer, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "m:%d{", len(keys))
	for _, k := range keys {
		fmt.Fprintf(w, "%d:%s=", len(k), k)
		canonicalizeFilterValue(w, m[k])
		fmt.Fprint(w, ",")
	}
	fmt.Fprint(w, "}")
}

package queries

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestCursor_RoundTrip_SingleElement(t *testing.T) {
	encoded, err := EncodeCursor([]any{"abc-123"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if len(got.K) != 1 || got.K[0] != "abc-123" {
		t.Fatalf("round-trip lost tuple, got %#v", got.K)
	}
}

func TestCursor_RoundTrip_MultiElement(t *testing.T) {
	encoded, err := EncodeCursor([]any{"Alice", "2024-01-01", "abc-123"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if len(got.K) != 3 {
		t.Fatalf("want tuple len 3, got %d (%#v)", len(got.K), got.K)
	}
	if got.K[0] != "Alice" || got.K[2] != "abc-123" {
		t.Fatalf("tuple contents drifted, got %#v", got.K)
	}
}

func TestCursor_RejectsEmpty(t *testing.T) {
	_, err := DecodeCursor("")
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for empty input, got %v", err)
	}
}

func TestCursor_RejectsCorruptBase64(t *testing.T) {
	_, err := DecodeCursor("not valid base64 !!!")
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for corrupt base64, got %v", err)
	}
}

func TestCursor_RejectsBase64ButCorruptJSON(t *testing.T) {
	bad := base64.URLEncoding.EncodeToString([]byte("not json"))
	_, err := DecodeCursor(bad)
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for corrupt JSON, got %v", err)
	}
}

func TestCursor_RejectsWrongVersion(t *testing.T) {
	// Manually craft a v=2 payload.
	payload := `{"v":2,"k":["abc"]}`
	bad := base64.URLEncoding.EncodeToString([]byte(payload))
	_, err := DecodeCursor(bad)
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for v=2, got %v", err)
	}
}

func TestCursor_RejectsMissingK(t *testing.T) {
	payload := `{"v":1}`
	bad := base64.URLEncoding.EncodeToString([]byte(payload))
	_, err := DecodeCursor(bad)
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for missing k, got %v", err)
	}
}

func TestCursor_RejectsEmptyK(t *testing.T) {
	payload := `{"v":1,"k":[]}`
	bad := base64.URLEncoding.EncodeToString([]byte(payload))
	_, err := DecodeCursor(bad)
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("want ErrCursorInvalid for empty k, got %v", err)
	}
}

func TestCursor_FilterHash_RoundTrip(t *testing.T) {
	hash := HashContext(map[string]any{"name": "Alice"}, nil, "", false)
	if hash == "" {
		t.Fatal("HashFilter of non-empty filter should not be empty")
	}
	encoded, err := EncodeCursor([]any{"abc-123"}, hash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got.H != hash {
		t.Fatalf("filter hash drift: got %q, want %q", got.H, hash)
	}
}

func TestCursor_EmptyFilterHashOmitted(t *testing.T) {
	encoded, err := EncodeCursor([]any{"abc-123"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got.H != "" {
		t.Fatalf("want empty H, got %q", got.H)
	}
}

func TestHashContext_DefaultContextProducesEmpty(t *testing.T) {
	if HashContext(nil, nil, "", false) != "" {
		t.Errorf("default context (nil filter, nil sort, empty search, no archive) must hash to empty string")
	}
	if HashContext(map[string]any{}, nil, "", false) != "" {
		t.Errorf("empty filter map must hash to empty string when other axes are default")
	}
}

func TestHashContext_DeterministicAcrossKeyOrder(t *testing.T) {
	f1 := map[string]any{"name": "Alice", "email": "a@x"}
	f2 := map[string]any{"email": "a@x", "name": "Alice"}
	if HashContext(f1, nil, "", false) != HashContext(f2, nil, "", false) {
		t.Fatal("filter hash must be stable regardless of insertion order")
	}
}

func TestHashContext_DistinguishesDifferentFilterValues(t *testing.T) {
	h1 := HashContext(map[string]any{"name": "Alice"}, nil, "", false)
	h2 := HashContext(map[string]any{"name": "Bob"}, nil, "", false)
	if h1 == h2 {
		t.Fatal("filters with different values must hash differently")
	}
}

func TestHashContext_DistinguishesTypesOnSameValue(t *testing.T) {
	// "1" (string) and 1 (int) must hash differently — otherwise a
	// type-mismatching consumer could craft a collision.
	h1 := HashContext(map[string]any{"x": "1"}, nil, "", false)
	h2 := HashContext(map[string]any{"x": int64(1)}, nil, "", false)
	if h1 == h2 {
		t.Fatal("string and int values must hash differently")
	}
}

func TestHashContext_WalksNestedMaps(t *testing.T) {
	h1 := HashContext(map[string]any{"age": map[string]any{"$gte": 18, "$lte": 65}}, nil, "", false)
	h2 := HashContext(map[string]any{"age": map[string]any{"$lte": 65, "$gte": 18}}, nil, "", false)
	if h1 != h2 {
		t.Fatal("nested map order must not affect the hash")
	}
}

func TestHashContext_WalksMultiClause(t *testing.T) {
	mc1 := MultiClause{Clauses: []any{
		map[string]any{"$gte": 22},
		map[string]any{"$lte": 27},
	}}
	mc2 := MultiClause{Clauses: []any{
		map[string]any{"$gte": 22},
		map[string]any{"$lte": 27},
	}}
	h1 := HashContext(map[string]any{"age": mc1}, nil, "", false)
	h2 := HashContext(map[string]any{"age": mc2}, nil, "", false)
	if h1 != h2 {
		t.Fatal("identical MultiClause filters must hash equally")
	}
	// Different clause order WOULD hash differently — MultiClause.Clauses is
	// an ordered slice and the wrapper produces it in URL order; this is by
	// design (predictable consumer behavior across replays of the same URL).
}

func TestHashContext_SortFieldChangeAltersHash(t *testing.T) {
	h1 := HashContext(nil, []SortField{{Field: "name"}}, "", false)
	h2 := HashContext(nil, []SortField{{Field: "email"}}, "", false)
	if h1 == h2 {
		t.Fatal("different sort fields must hash differently")
	}
}

func TestHashContext_SortDirectionChangeAltersHash(t *testing.T) {
	h1 := HashContext(nil, []SortField{{Field: "name", Desc: false}}, "", false)
	h2 := HashContext(nil, []SortField{{Field: "name", Desc: true}}, "", false)
	if h1 == h2 {
		t.Fatal("flipping a sort field's Desc must alter the hash")
	}
}

func TestHashContext_SortOrderChangeAltersHash(t *testing.T) {
	h1 := HashContext(nil, []SortField{{Field: "name"}, {Field: "email"}}, "", false)
	h2 := HashContext(nil, []SortField{{Field: "email"}, {Field: "name"}}, "", false)
	if h1 == h2 {
		t.Fatal("swapping the order of two sort fields must alter the hash (multi-key sort is direction-sensitive)")
	}
}

func TestHashContext_SearchChangeAltersHash(t *testing.T) {
	h1 := HashContext(nil, nil, "foo", false)
	h2 := HashContext(nil, nil, "bar", false)
	if h1 == h2 {
		t.Fatal("changing ?search= must alter the hash")
	}
	if h1 == "" || h2 == "" {
		t.Fatal("non-default search must produce non-empty hash")
	}
}

func TestHashContext_IncludeArchivedAltersHash(t *testing.T) {
	h1 := HashContext(nil, nil, "", false)
	h2 := HashContext(nil, nil, "", true)
	if h1 == h2 {
		t.Fatal("flipping includeArchived must alter the hash")
	}
	// Default-context hashes to empty; archived=true is non-default.
	if h1 != "" {
		t.Errorf("default context expected to hash to empty, got %q", h1)
	}
	if h2 == "" {
		t.Errorf("archived=true must produce non-empty hash, got empty")
	}
}

func TestHashContext_AllAxesCombineDistinctly(t *testing.T) {
	base := HashContext(map[string]any{"name": "Alice"},
		[]SortField{{Field: "email"}}, "foo", true)
	// Tweak each axis individually; every variant must differ from base.
	variants := []string{
		HashContext(map[string]any{"name": "Bob"},
			[]SortField{{Field: "email"}}, "foo", true),
		HashContext(map[string]any{"name": "Alice"},
			[]SortField{{Field: "name"}}, "foo", true),
		HashContext(map[string]any{"name": "Alice"},
			[]SortField{{Field: "email"}}, "bar", true),
		HashContext(map[string]any{"name": "Alice"},
			[]SortField{{Field: "email"}}, "foo", false),
	}
	for i, v := range variants {
		if v == base {
			t.Errorf("variant %d should differ from base hash", i)
		}
	}
}

func TestCursor_URLSafeEncoding(t *testing.T) {
	// The classic base64 alphabet uses '+' and '/' which get mangled in URL
	// query strings. URLEncoding swaps them for '-' and '_'. Verify the
	// encoded form contains no '+' or '/' for a payload that would otherwise
	// produce them under StdEncoding.
	// A binary-ish payload that hits the '+' / '/' alphabet slots:
	encoded, err := EncodeCursor([]any{"\xff\xfe\xfd"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	for _, r := range encoded {
		if r == '+' || r == '/' {
			t.Fatalf("encoded cursor must not contain '+' or '/' (URL-unsafe): %q", encoded)
		}
	}
}

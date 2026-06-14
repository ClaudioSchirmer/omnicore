package infra

import (
	"reflect"
	"testing"
)

func TestPascalToSnake(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Name", "name"},
		{"Email", "email"},
		{"CPF", "cpf"},
		{"ZipCode", "zip_code"},
		{"PostalCodeV2", "postal_code_v2"},
		{"HTTPStatus", "http_status"},
		{"OAuthToken", "o_auth_token"}, // acronym with a lowercase letter inside: documented behavior
		{"ID", "id"},
		{"UserID", "user_id"},
		{"", ""},
		{"A", "a"},
		{"AB", "ab"},
		{"Ab", "ab"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := pascalToSnake(c.in)
			if got != c.want {
				t.Errorf("pascalToSnake(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

type sampleFlat struct {
	Name  string
	Email string
	CPF   string
	Phone *string
}

type sampleWithTags struct {
	Name      string `db:"display_name"` // db: tag is ignored (column override lives in RepoConfig)
	Email     string
	Skipped   string `transient:"-"` // domain marks the field as not persisted; infra skips it
	WithBlank string `db:""`         // db: tag ignored
}

type sampleWithEmbed struct {
	sampleFlat        // anonymous embed — must be ignored
	Extra      string `db:"extra"`
}

type samplePrivateFields struct {
	Name   string
	secret string //nolint:unused // intentional for testing
}

func TestDomainColumns_Flat(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(sampleFlat{}))
	want := []string{"name", "email", "cpf", "phone"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v", cols, want)
	}
}

func TestDomainColumns_PtrAlsoWorks(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(&sampleFlat{}))
	want := []string{"name", "email", "cpf", "phone"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v", cols, want)
	}
}

func TestDomainColumns_Tags(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(sampleWithTags{}))
	// Tag `db:` is fully ignored (column override lives in RepoConfig).
	// Name becomes "name" (snake_case of the field); Email "email"; Skipped
	// skipped via tag `transient:"-"`; WithBlank "with_blank" (snake_case of
	// the field — the empty `db:` tag does not override).
	want := []string{"name", "email", "with_blank"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v", cols, want)
	}
}

func TestDomainColumns_IgnoresAnonymousEmbed(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(sampleWithEmbed{}))
	want := []string{"extra"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v (embed sampleFlat must be ignored)", cols, want)
	}
}

func TestDomainColumns_IgnoresPrivateFields(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(samplePrivateFields{}))
	want := []string{"name"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v", cols, want)
	}
}

func TestDomainColumns_NonStructReturnsNil(t *testing.T) {
	if got := domainColumns(reflect.TypeOf(42)); got != nil {
		t.Errorf("non-struct must return nil, got %v", got)
	}
	if got := domainColumns(reflect.TypeOf("foo")); got != nil {
		t.Errorf("non-struct must return nil, got %v", got)
	}
}

// sampleTransient declares one field of each "shape" the transient tag is
// meant to mark — request-scoped input, computed/derived value, in-memory
// cache, runtime bookkeeping flag. All four must drop out of the column set
// regardless of declaration order or the surrounding field types.
type sampleTransient struct {
	Name             string
	RequestEmail     string `transient:"-"` // request input
	DerivedFullName  string `transient:"-"` // computed at runtime
	LastLookupCached *int   `transient:"-"` // in-memory cache
	AttemptCounter   int    `transient:"-"` // runtime bookkeeping
	Email            string
}

func TestDomainColumns_TransientTagSkipsField(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(sampleTransient{}))
	want := []string{"name", "email"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v — every transient:\"-\" field must be skipped regardless of its kind/usage", cols, want)
	}
}

// sampleTransientOtherValue documents that the parser is strict on the
// hyphen convention (mirrors `json:"-"`). A value other than `-` does NOT
// skip the field — keeps the contract narrow and avoids accidental skips
// from misuse (`transient:"true"` would silently match a future loose check).
type sampleTransientOtherValue struct {
	Name  string
	Other string `transient:"true"` // NOT a valid skip signal — only `-` skips
}

func TestDomainColumns_TransientTagRequiresHyphen(t *testing.T) {
	cols := domainColumns(reflect.TypeOf(sampleTransientOtherValue{}))
	want := []string{"name", "other"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("got %v, want %v — only `transient:\"-\"` skips; other values are ignored", cols, want)
	}
}

func TestStructIndex_CachesByType(t *testing.T) {
	t1 := reflect.TypeOf(sampleFlat{})
	a := loadStructIndex(t1)
	b := loadStructIndex(t1)
	if a != b {
		t.Errorf("loadStructIndex should return the same cached ptr, got %p and %p", a, b)
	}
}

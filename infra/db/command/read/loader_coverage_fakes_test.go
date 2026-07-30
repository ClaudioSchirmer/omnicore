package read

import "github.com/ClaudioSchirmer/omnicore/domain"

// Coverage-oriented fakes shared by the aggregate-loader coverage suites.
//
// selectiveDecodeDialect is testPGDialect with a scriptable DecodeID failure:
// failOn == "" fails on every raw value; otherwise only the matching raw value
// fails and everything else passes through. It drives the loader's per-branch
// DecodeID error handling (root id, child ParentID, child own ID, base id) without a
// real engine — the same embed-and-override pattern mysqlLikeDialect uses.
type selectiveDecodeDialect struct {
	testPGDialect
	failOn string
}

func (d selectiveDecodeDialect) DecodeID(raw string) (string, error) {
	if d.failOn == "" || raw == d.failOn {
		return "", errFakeDB
	}
	return raw, nil
}

// decodeErrEngine is fakeRelEngine with the Dialect overridden, so a loader can
// run the scriptable read seam while DecodeID fails on demand.
type decodeErrEngine struct {
	fakeRelEngine
	dialect Dialect
}

func (e decodeErrEngine) Dialect() Dialect { return e.dialect }

func decodeErrFakeEngine(queryFn func(sql string, args []any) (Rows, error), failOn string) RelationalEngine {
	return decodeErrEngine{
		fakeRelEngine: fakeRelEngine{q: fakeQuerier{queryFn: queryFn}},
		dialect:       selectiveDecodeDialect{failOn: failOn},
	}
}

// noColsChild is an AggregateValueObject whose schema resolves ZERO scan
// columns: it declares no Field(...) — the shape the loader's "schema declares
// no columns" guards fire on. The id lives in the promoted domain.Managed carrier.
type noColsChild struct{ domain.Managed }

func (noColsChild) BuildRules(string, domain.Service, *domain.Rules) {}

func noColsChildSchema(fkCol string) *TableSchema {
	return NewTableSchema[noColsChild]("no_cols_children").ID("id").ParentID(fkCol)
}

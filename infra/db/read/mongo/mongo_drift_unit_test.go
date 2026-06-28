package mongo

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// These tests drive DetectViewDrift + hasUserDocuments end-to-end against the
// in-process fakes. DetectViewDrift reads the registry through the engine's
// neutral Querier; the fake's Query is programmed so ReadViewRegistry returns
// "no row" (Next()==false) or an error. The Mongo collection's CountDocuments is
// programmed via fakeColl.count / countErr. decideDrift itself is covered
// elsewhere — here we exercise the orchestration + error wiring.

func driftView() *ViewDefinition {
	return View("orders").Version(1).Root("orders").Schema(composerRootSchema())
}

// noRegistryRowEngine yields zero registry rows from Query, so ReadViewRegistry
// returns (nil, nil) — the "no registry row" branch.
func noRegistryRowEngine() db.RelationalEngine {
	return newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (db.Rows, error) { return &fakeRows{}, nil },
	})
}

func TestDetectViewDrift_FreshInit_NoRowEmptyMongo(t *testing.T) {
	mongo := newFakeMongo(&fakeColl{count: 0})

	report, err := DetectViewDrift(newBuilderCtx(), mongo, noRegistryRowEngine(), []*ViewDefinition{driftView()})
	if err != nil {
		t.Fatalf("DetectViewDrift: %v", err)
	}
	if len(report.Plans) != 1 || report.Plans[0].Decision != DriftFreshInit {
		t.Fatalf("decision = %v, want DriftFreshInit", report.Plans[0].Decision)
	}
}

func TestDetectViewDrift_AlienData_NoRowPopulatedMongo(t *testing.T) {
	mongo := newFakeMongo(&fakeColl{count: 1})

	report, err := DetectViewDrift(newBuilderCtx(), mongo, noRegistryRowEngine(), []*ViewDefinition{driftView()})
	if err != nil {
		t.Fatalf("DetectViewDrift: %v", err)
	}
	if report.Plans[0].Decision != DriftAlienData {
		t.Fatalf("decision = %v, want DriftAlienData", report.Plans[0].Decision)
	}
}

func TestDetectViewDrift_RegistryReadError(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (db.Rows, error) { return nil, errFake },
	})
	mongo := newFakeMongo(&fakeColl{count: 0})

	if _, err := DetectViewDrift(newBuilderCtx(), mongo, eng, []*ViewDefinition{driftView()}); err == nil {
		t.Fatal("expected registry read error")
	}
}

func TestDetectViewDrift_HasUserDocumentsError(t *testing.T) {
	mongo := newFakeMongo(&fakeColl{countErr: errFake})

	if _, err := DetectViewDrift(newBuilderCtx(), mongo, noRegistryRowEngine(), []*ViewDefinition{driftView()}); err == nil {
		t.Fatal("expected hasUserDocuments count error")
	}
}

func TestHasUserDocuments_CountBranches(t *testing.T) {
	mongo := newFakeMongo(&fakeColl{count: 0})
	if got, err := hasUserDocuments(newBuilderCtx(), mongo, "orders"); err != nil || got {
		t.Fatalf("empty: got=%v err=%v, want false,nil", got, err)
	}
	mongoPop := newFakeMongo(&fakeColl{count: 5})
	if got, err := hasUserDocuments(newBuilderCtx(), mongoPop, "orders"); err != nil || !got {
		t.Fatalf("populated: got=%v err=%v, want true,nil", got, err)
	}
	mongoErr := newFakeMongo(&fakeColl{countErr: errFake})
	if _, err := hasUserDocuments(newBuilderCtx(), mongoErr, "orders"); err == nil {
		t.Fatal("expected count error")
	}
}

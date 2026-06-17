package handlers

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// testEntity is a minimal Entity used by all *_test.go in this package.
// BuildRulesSeenService captures the last Service received in BuildRules;
// Service injection tests via Auto handler inspect this field.
type testEntity struct {
	domain.BaseEntity
	Name                  string
	BuildRulesSeenService domain.Service
	BuildRulesCalled      bool
}

func (e *testEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *testEntity) RequiresService() bool   { return false }
func (e *testEntity) TableName() string       { return "test" }
func (e *testEntity) ToFields() domain.Fields { return domain.Fields{"name": e.Name} }
func (e *testEntity) BuildRules(_ string, svc domain.Service, _ *domain.Rules) {
	e.BuildRulesCalled = true
	e.BuildRulesSeenService = svc
}

// testService implements domain.Service to verify that the handler's
// Service reaches the entity's BuildRules.
type testService struct{ domain.ServiceBase }

// testCmdWithID covers Archive/Delete/Unarchive Commands. The framework's
// post-ctx interfaces require ApplyTo(ctx, t) + FromEntity(ctx, t) on all
// 6 verbs — for Archive/Unarchive/Delete the typical shape is "no projection",
// returning results.None from FromEntity. The test stub is a no-op on
// ApplyTo (no transient field on testEntity) and returns results.None{} from
// FromEntity, proving the wiring without depending on any specific business
// invariant.
type testCmdWithID struct {
	pipeline.CommandBaseWithID
}

func (c *testCmdWithID) ApplyTo(_ *configuration.AppContext, _ *testEntity)              {}
func (c *testCmdWithID) FromEntity(_ *configuration.AppContext, _ *testEntity) results.None { return results.None{} }

// mockRepo implements persistence.ScopedRepository[*testEntity]. Counters
// track calls; nil error fields make each method succeed by default. The
// writes live on the mockWriter that Scope returns; each write captures the
// variadic []WriteOption[*testEntity] (folded into the call at Scope time)
// so the Auto-handler dispatch tests can assert which provider closures the
// handler threaded.
type mockRepo struct {
	insertCalled    int
	updateCalled    int
	deleteCalled    int
	archiveCalled   int
	unarchiveCalled int
	findByIDCalled  int

	insertErr    error
	updateErr    error
	deleteErr    error
	archiveErr   error
	unarchiveErr error
	findErr      error

	insertOpts    []persistence.WriteOption[*testEntity]
	updateOpts    []persistence.WriteOption[*testEntity]
	deleteOpts    []persistence.WriteOption[*testEntity]
	archiveOpts   []persistence.WriteOption[*testEntity]
	unarchiveOpts []persistence.WriteOption[*testEntity]

	foundData *testEntity
}

func newMockRepo() *mockRepo {
	e := &testEntity{Name: "found"}
	e.SetID(domain.NewID(uuid.NewString()))
	return &mockRepo{foundData: e}
}

// Scope binds the opts at call time and returns the mockWriter that
// records each write against the parent mockRepo.
func (r *mockRepo) Scope(_ *configuration.AppContext, opts ...persistence.WriteOption[*testEntity]) domain.Writer {
	return &mockWriter{repo: r, opts: opts}
}

// mockWriter is the request-scoped domain.Writer mockRepo.Scope returns.
type mockWriter struct {
	repo *mockRepo
	opts []persistence.WriteOption[*testEntity]
}

func (w *mockWriter) Insert(_ domain.Insertable) (domain.ID, error) {
	w.repo.insertCalled++
	w.repo.insertOpts = w.opts
	if w.repo.insertErr != nil {
		return domain.ID{}, w.repo.insertErr
	}
	return domain.NewRandomID(), nil
}

func (w *mockWriter) Update(_ domain.Updatable) error {
	w.repo.updateCalled++
	w.repo.updateOpts = w.opts
	return w.repo.updateErr
}

func (w *mockWriter) Delete(_ domain.Deletable) error {
	w.repo.deleteCalled++
	w.repo.deleteOpts = w.opts
	return w.repo.deleteErr
}

func (w *mockWriter) Archive(_ domain.Archivable) error {
	w.repo.archiveCalled++
	w.repo.archiveOpts = w.opts
	return w.repo.archiveErr
}

func (w *mockWriter) Unarchive(_ domain.Unarchivable) error {
	w.repo.unarchiveCalled++
	w.repo.unarchiveOpts = w.opts
	return w.repo.unarchiveErr
}

func (r *mockRepo) FindByID(domain.ID) (*testEntity, error) {
	r.findByIDCalled++
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.foundData, nil
}

func (r *mockRepo) New() *testEntity {
	return &testEntity{}
}

func testCtx() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangPTBR)
}

// spyReader records calls into ReadPage/ReadByID and returns canned values.
// Used by find_by_params_test.go and find_by_id_test.go.
type spyReader struct {
	readPageCalled int
	readByIDCalled int

	gotView     string
	gotCriteria queries.ReadCriteria
	gotID       string

	pageToReturn queries.Page
	pageErr      error
	docToReturn  map[string]any
	docFound     bool
	docErr       error
}

func (s *spyReader) ReadPage(_ context.Context, view string, c queries.ReadCriteria) (queries.Page, error) {
	s.readPageCalled++
	s.gotView = view
	s.gotCriteria = c
	return s.pageToReturn, s.pageErr
}

func (s *spyReader) ReadByID(_ context.Context, view, id string, c queries.ReadCriteria) (map[string]any, bool, error) {
	s.readByIDCalled++
	s.gotView = view
	s.gotID = id
	s.gotCriteria = c
	return s.docToReturn, s.docFound, s.docErr
}

// testFindParamsQuery is a minimal FindByParamsQuery for handler tests:
// echoes a Criteria captured at construction time. The recorder lets the
// test assert that the handler passes the request ctx into ToCriteria.
type testFindParamsQuery struct {
	queries.ReadCriteria
	pipeline.QueryBase
	gotCtx *configuration.AppContext
}

func (q *testFindParamsQuery) ToCriteria(ctx *configuration.AppContext) queries.ReadCriteria {
	q.gotCtx = ctx
	return q.ReadCriteria
}

// testFindIDQuery is a minimal FindByIDQuery for handler tests. Honors
// the Query-side ToCriteria(ctx) contract and records ctx so tests can
// assert ctx propagation. Mirrors the behavior of FindUserByIDQuery in
// the canonical example.
type testFindIDQuery struct {
	queries.QueryBaseWithID
	includeArchived bool
	contextName     string
	overlay         map[string]any
	gotCtx          *configuration.AppContext
}

func (q *testFindIDQuery) ToCriteria(ctx *configuration.AppContext) queries.ReadCriteria {
	q.gotCtx = ctx
	return queries.ReadCriteria{
		IncludeArchived: q.includeArchived,
		Filter:          q.overlay,
	}
}
func (q testFindIDQuery) ContextName() string { return q.contextName }

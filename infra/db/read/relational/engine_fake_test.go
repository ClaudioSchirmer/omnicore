package relational

import (
	"context"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// fakeRelEngine is a minimal db.RelationalEngine for white-box tests. It needs
// the engine only for its Dialect (e.g. BaseRepository.mapErr's unique-violation
// classification) and, optionally, a scriptable read seam (q) the loader runs
// SELECTs through. Its Dialect mirrors Postgres's SQLSTATE 23505 detection via
// testPGDialect; the write verbs are never reached on these paths.
type fakeRelEngine struct{ q Querier }

func (fakeRelEngine) Insert(persistence.RequestContext, domain.Insertable, *TableSchema, WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (fakeRelEngine) Update(persistence.RequestContext, domain.Updatable, *TableSchema, WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (fakeRelEngine) Archive(persistence.RequestContext, domain.Archivable, *TableSchema, WriteHook) error {
	return nil
}
func (fakeRelEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *TableSchema, WriteHook) error {
	return nil
}
func (fakeRelEngine) Delete(persistence.RequestContext, domain.Deletable, *TableSchema, WriteHook) error {
	return nil
}
func (e fakeRelEngine) Querier() Querier { return e.q }
func (fakeRelEngine) Dialect() Dialect   { return testPGDialect{} }
func (e fakeRelEngine) WithAudit(*audit.Config, *slog.Logger, []string) RelationalEngine {
	return e
}
func (e fakeRelEngine) WithEventPublisher(events.Publisher) RelationalEngine { return e }
func (e fakeRelEngine) AcquireRebuildLock(context.Context, string) (RebuildLock, error) {
	return fakeRebuildLock{q: e.q}, nil
}
func (fakeRelEngine) Close() {}

// fakeRebuildLock is the no-op RebuildLock the fake engine hands back: always
// acquired, its Querier the engine's scriptable read seam.
type fakeRebuildLock struct{ q Querier }

func (fakeRebuildLock) Acquired() bool             { return true }
func (fakeRebuildLock) Holder() string             { return "" }
func (l fakeRebuildLock) Querier() Querier          { return l.q }
func (fakeRebuildLock) Release(context.Context) error { return nil }

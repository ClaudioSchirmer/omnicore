package mongo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RegistryCollectionName is the framework-owned collection where each
// service writes a per-boot marker so the DB-per-service guard can detect
// when a database is being shared across services. The name uses the
// "omnicore_" prefix so it cannot collide with any service-declared
// query.ViewDefinition collection name (a service collection name never starts with
// the reserved "omnicore_" prefix).
const RegistryCollectionName = "omnicore_service_registry"

// DevProfile is the single profile name under which the guard downgrades
// foreign-collection detection to a slog.Warn. Mirrors the rule the
// bootstrap layer applies to auth.mode=disabled — "dev" is the canonical
// loose-rules profile; everything else is strict.
const DevProfile = "dev"

// serviceMarker is the document each service upserts under
// RegistryCollectionName on every boot. Identity stays under the service
// name (one document per service in this database); the other fields are
// refreshed on each boot for inspection.
type serviceMarker struct {
	ID        string    `bson:"_id"`
	ProcessID string    `bson:"process_id"`
	BootAt    time.Time `bson:"boot_at"`
	PID       int       `bson:"pid"`
	Host      string    `bson:"host"`
}

// CheckServiceRegistry enforces the DB-per-service boundary at boot. The
// service writes a marker document under RegistryCollectionName, then
// lists every collection in the database and confirms the set matches
// what the framework expects to manage: the declared views, the local
// mirrors the declared upstream subscriptions materialize, the registry
// itself, and the implicit `system.*` namespace Mongo owns.
//
// upstreamCollections is that second group — one name per declared
// upstreamSubscriptions entry. A mirror is written by the framework, on
// this service's behalf, into this service's own database: it is claimed
// exactly like a view's collection, and omitting it would report a
// service's own data as another tenant's residue. It carries no
// blue-green slots — no registry row resolves it, so the subscriber only
// ever writes the bare name.
//
// Foreign collections (anything outside that set) signal that the
// database is shared with another service OR carries residue from a
// previous service iteration. The guard's response depends on profile:
//
//   - profile == "dev" → slog.Warn naming each foreign collection. Boot
//     continues. Lets hot-reload + ad-hoc mongosh work locally without
//     fighting the developer.
//   - any other profile → return a typed error naming each foreign
//     collection. Boot aborts. The operator owns the resolution
//     (drop residue, point service at a clean DB, or extend the
//     declared view set to cover the orphan).
//
// Always idempotent on the marker side: a service rebooting against a
// database where its own marker already exists just updates ProcessID /
// BootAt / PID / Host in place.
func CheckServiceRegistry(
	ctx context.Context,
	m *MongoDB,
	serviceName string,
	profile string,
	views []*query.ViewDefinition,
	upstreamCollections []string,
) error {
	if serviceName == "" {
		return fmt.Errorf("CheckServiceRegistry: serviceName must not be empty")
	}

	if err := upsertServiceMarker(ctx, m, serviceName); err != nil {
		return fmt.Errorf("upsert service marker: %w", err)
	}

	foreign, otherServices, err := scanForeignCollections(ctx, m, serviceName, views, upstreamCollections)
	if err != nil {
		return fmt.Errorf("scan collections: %w", err)
	}

	if len(otherServices) > 0 {
		// Co-tenants of the same DB are surfaced unconditionally — useful
		// for inspection on clusters that deliberately share a DB across
		// non-conflicting services, but the warning runs in every
		// profile because the cost of staying silent is high (silent
		// collision when one service adds an index the other already
		// owns).
		slog.WarnContext(ctx, "mongo.registry.other_services_present",
			slog.String("service", serviceName),
			slog.Any("other_services", otherServices))
	}

	return decideForeignResponse(ctx, serviceName, m.db.Name(), profile, foreign)
}

// decideForeignResponse implements the warn-em-dev / abort-fora branch
// of the guard, separated from CheckServiceRegistry so the closed-set
// behavior is unit-testable without an active Mongo connection. Returns
// nil when there is nothing to report or the profile is "dev"; returns
// a typed error naming each foreign collection otherwise.
func decideForeignResponse(ctx context.Context, serviceName, database, profile string, foreign []string) error {
	if len(foreign) == 0 {
		return nil
	}
	if profile == DevProfile {
		slog.WarnContext(ctx, "mongo.registry.foreign_collections",
			slog.String("service", serviceName),
			slog.String("database", database),
			slog.String("profile", profile),
			slog.Any("foreign", foreign),
			slog.String("reason", "downgraded to warn under dev profile"))
		return nil
	}
	return errors.New(foreignCollectionsDiagnostic(serviceName, database, foreign))
}

// foreignCollectionsDiagnostic writes the abort message. It is long on purpose:
// the operator reading it is looking at a database they did not expect to be
// wrong, and the single most common cause is invisible from where they stand —
// THE VIEW'S DECLARATION IS GONE FROM THE BUILD. Data outlives code. A
// query.View(...) that was deleted, renamed, or converted to
// query.RelationalView(...) stops claiming its collection the moment the source
// changes, while the collection itself sits there fully populated. The framework
// will not drop it on its own: from here a collection nobody declares is
// indistinguishable from another service's live data, and dropping the wrong one
// is unrecoverable.
//
// So the message names the database, names each collection, explains the likely
// cause, and hands over the exact two statements to run — the mongosh drop AND
// the relational bookkeeping row, which is keyed by the VIEW name and would
// otherwise be missed (a stale row later resolves as DriftAlienData and aborts
// the boot a second time, for a different reason).
func foreignCollectionsDiagnostic(serviceName, database string, foreign []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "service %q: database %q holds %d collection(s) that no declaration of this service claims: %s\n\n",
		serviceName, database, len(foreign), strings.Join(foreign, ", "))

	sb.WriteString("The usual cause is that the read model's DECLARATION IS NO LONGER IN THIS BUILD — its\n")
	sb.WriteString("query.View(...) was deleted, renamed, or converted to query.RelationalView(...), which\n")
	sb.WriteString("materializes nothing and therefore claims no collection. The data outlives the source:\n")
	sb.WriteString("the collection stays behind, populated, and the framework will not drop it for you,\n")
	sb.WriteString("because from here it is indistinguishable from another service sharing this database.\n\n")
	sb.WriteString("Resolve by ONE of:\n\n")

	sb.WriteString("  A. Drop the residue — it is no longer read by anything. In mongosh:\n")
	fmt.Fprintf(&sb, "       use %s\n", database)
	for _, c := range foreign {
		fmt.Fprintf(&sb, "       db.getCollection(%q).drop()\n", c)
	}
	sb.WriteString("     then delete each one's bookkeeping row in the RELATIONAL store (keyed by the\n")
	sb.WriteString("     VIEW name — the collection name without any __0 / __1 blue-green suffix):\n")
	for _, view := range distinctViewNames(foreign) {
		fmt.Fprintf(&sb, "       DELETE FROM omnicore_mongo_views WHERE view_name = '%s';\n", view)
	}
	sb.WriteString("     Leaving that row behind aborts the next boot for a different reason.\n\n")

	sb.WriteString("  B. Give this service a database of its own — set mongo.database in\n")
	sb.WriteString("     microservice.<profile>.yaml. Correct when another service legitimately shares\n")
	sb.WriteString("     this one; the DB-per-service boundary is what this guard exists to hold.\n\n")

	sb.WriteString("  C. Re-declare it, if the read model is still supposed to be served: contribute the\n")
	sb.WriteString("     query.View(...) again from a ReadableFeature's Views() — or, for a mirror of\n")
	sb.WriteString("     another service's data, the upstreamSubscriptions entry whose collection: names\n")
	sb.WriteString("     it. A subscription claims its local collection exactly like a view does.\n")
	return sb.String()
}

// distinctViewNames maps the foreign collections back to the view rows they came
// from, de-duplicated and in first-seen order: a view's two blue-green slots are
// two collections but ONE registry row, so emitting the DELETE twice would read
// as two different problems.
func distinctViewNames(foreign []string) []string {
	seen := make(map[string]struct{}, len(foreign))
	out := make([]string, 0, len(foreign))
	for _, c := range foreign {
		v := query.ViewNameOf(c)
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// upsertServiceMarker writes / refreshes the per-boot marker under the
// service name. Identity is the service name so a reboot updates in
// place; the ProcessID is the discriminator that lets log correlation
// distinguish one boot from another.
func upsertServiceMarker(ctx context.Context, m *MongoDB, serviceName string) error {
	host, _ := os.Hostname()
	marker := serviceMarker{
		ID:        serviceName,
		ProcessID: uuid.NewString(),
		BootAt:    time.Now().UTC(),
		PID:       os.Getpid(),
		Host:      host,
	}
	col := m.db.Collection(RegistryCollectionName)
	_, err := col.ReplaceOne(ctx,
		bson.M{"_id": serviceName},
		marker,
		options.Replace().SetUpsert(true))
	return err
}

// scanForeignCollections enumerates every collection in the database and
// returns:
//
//   - foreign: collections outside the "framework-managed" set (declared
//     views + upstream-subscription mirrors + registry + system.*
//     namespace).
//   - otherServices: distinct _id values present in
//     RegistryCollectionName other than this service. Logged
//     unconditionally so operators are aware the database carries
//     other tenants even when no foreign collection is found.
//
// The function never aborts itself — it returns the diff and lets the
// caller decide how to react based on profile. The set comparison is
// delegated to filterForeignCollections (pure, unit-tested).
func scanForeignCollections(
	ctx context.Context,
	m *MongoDB,
	serviceName string,
	views []*query.ViewDefinition,
	upstreamCollections []string,
) (foreign []string, otherServices []string, err error) {
	names, err := m.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return nil, nil, err
	}
	foreign = filterForeignCollections(names, views, upstreamCollections)
	otherServices, err = listOtherServices(ctx, m, serviceName)
	if err != nil {
		return foreign, nil, err
	}
	return foreign, otherServices, nil
}

// filterForeignCollections is the pure subset of scanForeignCollections:
// given the observed collection names, the declared views and the local
// mirrors the declared upstream subscriptions materialize, return the
// names outside the framework-managed set (those two + framework-owned
// collections + `system.*` namespace). Output is sorted for
// deterministic error / log diagnostics.
func filterForeignCollections(observed []string, views []*query.ViewDefinition, upstreamCollections []string) []string {
	declared := make(map[string]struct{}, len(views)*3+len(upstreamCollections)+1)
	for _, v := range views {
		// A view can physically live in the bare <view> OR either blue-green slot
		// (<view>__0 / <view>__1). Whitelist all three, or the guard flags the
		// view's own active/shadow slot as a foreign orphan and aborts a non-dev boot.
		for _, name := range query.PhysicalCollectionNames(v.Name()) {
			declared[name] = struct{}{}
		}
	}
	// An upstream mirror is claimed under its bare name only: it has no
	// omnicore_mongo_views row, so the resolver never points it at a slot and the
	// subscriber writes nowhere else. Whitelisting slots it cannot own would only
	// hide residue.
	for _, name := range upstreamCollections {
		if name == "" {
			continue
		}
		declared[name] = struct{}{}
	}
	for _, name := range frameworkOwnedCollections() {
		declared[name] = struct{}{}
	}
	var foreign []string
	for _, n := range observed {
		if _, ok := declared[n]; ok {
			continue
		}
		if strings.HasPrefix(n, "system.") {
			continue
		}
		foreign = append(foreign, n)
	}
	sort.Strings(foreign)
	return foreign
}

// listOtherServices returns the distinct service names present in
// RegistryCollectionName other than the current service. A fresh
// database (no registry yet) returns an empty slice without error.
func listOtherServices(ctx context.Context, m *MongoDB, serviceName string) ([]string, error) {
	col := m.db.Collection(RegistryCollectionName)
	cur, err := col.Find(ctx, bson.M{"_id": bson.M{"$ne": serviceName}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if id, ok := d["_id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// frameworkOwnedCollections is the list of collection names the
// framework reserves for its own use. Centralized here so a future
// addition (telemetry buffer, lease table, …) only updates one place.
func frameworkOwnedCollections() []string {
	// The projection-state registry (base-revision records + document tombstones) is
	// materialized by the SyncEngine itself (EnsureProjectionState) — a
	// framework-owned collection exactly like the service marker, never a
	// foreign orphan.
	return []string{RegistryCollectionName, query.ProjectionStateCollectionName}
}

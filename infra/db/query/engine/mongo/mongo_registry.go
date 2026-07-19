package mongo

import (
	"context"
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
// what the framework expects to manage: the declared views, the
// registry itself, and the implicit `system.*` namespace Mongo owns.
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
) error {
	if serviceName == "" {
		return fmt.Errorf("CheckServiceRegistry: serviceName must not be empty")
	}

	if err := upsertServiceMarker(ctx, m, serviceName); err != nil {
		return fmt.Errorf("upsert service marker: %w", err)
	}

	foreign, otherServices, err := scanForeignCollections(ctx, m, serviceName, views)
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

	return decideForeignResponse(ctx, serviceName, profile, foreign)
}

// decideForeignResponse implements the warn-em-dev / abort-fora branch
// of the guard, separated from CheckServiceRegistry so the closed-set
// behavior is unit-testable without an active Mongo connection. Returns
// nil when there is nothing to report or the profile is "dev"; returns
// a typed error naming each foreign collection otherwise.
func decideForeignResponse(ctx context.Context, serviceName, profile string, foreign []string) error {
	if len(foreign) == 0 {
		return nil
	}
	if profile == DevProfile {
		slog.WarnContext(ctx, "mongo.registry.foreign_collections",
			slog.String("service", serviceName),
			slog.String("profile", profile),
			slog.Any("foreign", foreign),
			slog.String("reason", "downgraded to warn under dev profile"))
		return nil
	}
	return fmt.Errorf(
		"service %q: foreign collections present in database (not declared by any view): %s — "+
			"another service may be sharing this database, or these are residue from a prior service iteration; "+
			"resolve by dropping the orphan collections, pointing the service at a clean database, or "+
			"declaring them via fwinfra.View(...)",
		serviceName, strings.Join(foreign, ", "))
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
//     views + registry + system.* namespace).
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
) (foreign []string, otherServices []string, err error) {
	names, err := m.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return nil, nil, err
	}
	foreign = filterForeignCollections(names, views)
	otherServices, err = listOtherServices(ctx, m, serviceName)
	if err != nil {
		return foreign, nil, err
	}
	return foreign, otherServices, nil
}

// filterForeignCollections is the pure subset of scanForeignCollections:
// given the observed collection names and the declared views, return
// the names outside the framework-managed set (declared views +
// framework-owned collections + `system.*` namespace). Output is sorted
// for deterministic error / log diagnostics.
func filterForeignCollections(observed []string, views []*query.ViewDefinition) []string {
	declared := make(map[string]struct{}, len(views)*3+1)
	for _, v := range views {
		// A view can physically live in the bare <view> OR either blue-green slot
		// (<view>__0 / <view>__1). Whitelist all three, or the guard flags the
		// view's own active/shadow slot as a foreign orphan and aborts a non-dev boot.
		for _, name := range query.PhysicalCollectionNames(v.Name()) {
			declared[name] = struct{}{}
		}
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
	return []string{RegistryCollectionName}
}

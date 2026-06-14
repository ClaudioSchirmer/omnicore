package infra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EnvForceRebuild is the process-environment switch operators flip to
// authorize the apply step to drop divergent indexes and recreate them
// from the declared spec. NOT auto-enabled in any profile — explicit
// operator opt-in, typically right before a targeted deploy with a
// schema-evolution change. Scope is intentionally narrow:
//
//   - Indexes: divergent specs are dropped and recreated.
//   - Collections: never dropped (would lose user data). Collection-level
//     divergence (collation, capped, time-series) is always strict; the
//     operator owns the destructive migration path.
//   - Validator ($jsonSchema): never destructive — collMod is idempotent
//     and rewrites the validator in place regardless of this flag.
const EnvForceRebuild = "OMNICORE_MONGO_FORCE_REBUILD"

// Mongo error codes the apply step reacts to. Codes are stable wire
// constants in Mongo's protocol and do not change across driver
// versions.
//
// See https://github.com/mongodb/mongo/blob/master/src/mongo/base/error_codes.yml
const (
	mongoErrIndexOptionsConflict = 85
	mongoErrIndexKeySpecsConflict = 86
)

// ApplyMongoSpecs walks the declared views and brings the Mongo cluster
// to the state each ViewDefinition asks for: creates absent collections
// with declared options, applies the $jsonSchema validator via collMod,
// and ensures every declared index exists with the declared spec.
//
// Idempotent: when every artifact already matches its declaration, the
// call performs only read-side round-trips (listCollections +
// CreateOne-with-identical-spec absorbed by the driver) and returns nil.
//
// Strict on collection-level divergence (collation / capped /
// time-series): aborts with a diagnostic naming the view and the
// offending option. Mongo treats these as immutable on existing
// collections — the apply step does NOT auto-drop the collection; the
// operator owns the migration path.
//
// Index divergence default: aborts with a diagnostic carrying the
// driver's IndexOptionsConflict / IndexKeySpecsConflict error.
// When OMNICORE_MONGO_FORCE_REBUILD=true is set in the process env, the
// apply step drops the divergent index and recreates it from the
// declared spec. Single opt-in flag covers all index divergences in the
// round; never auto-enabled by any profile.
//
// Views with no declared mongo spec (zero indexes, no validator, no
// collation / capped / time-series) only trigger a listCollections lookup
// to confirm the collection exists or is creatable — the framework does
// not touch it beyond that.
func ApplyMongoSpecs(ctx context.Context, m *MongoDB, views []*ViewDefinition) error {
	forceRebuild := os.Getenv(EnvForceRebuild) == "true"
	slog.InfoContext(ctx, "mongo.apply.start",
		slog.Int("views", len(views)),
		slog.Bool("forceRebuild", forceRebuild))

	for _, v := range views {
		if err := v.ValidateMongoSpec(); err != nil {
			return err
		}
		if err := applyOneView(ctx, m, v, forceRebuild); err != nil {
			return err
		}
	}

	slog.InfoContext(ctx, "mongo.apply.end", slog.Int("views", len(views)))
	return nil
}

// applyOneView is the per-view sequence. Order matters:
//
//  1. listCollections — see what's there.
//  2. createCollection OR assert match — bring shape to declared.
//  3. collMod validator — update validator on an existing collection
//     (skipped when we just created it with the validator embedded).
//  4. createIndexes — ensure every declared index exists.
func applyOneView(ctx context.Context, m *MongoDB, v *ViewDefinition, forceRebuild bool) error {
	exists, observed, err := lookupCollection(ctx, m, v.Name())
	if err != nil {
		return fmt.Errorf("view %q: listCollections failed: %w", v.Name(), err)
	}

	if !exists {
		if err := createCollectionWithSpec(ctx, m, v); err != nil {
			return fmt.Errorf("view %q: createCollection failed: %w", v.Name(), err)
		}
		// Validator already embedded in createCollection — skip collMod.
	} else {
		if err := assertCollectionMatches(v, observed); err != nil {
			return err
		}
		if v.SchemaSpec() != nil {
			if err := applyValidator(ctx, m, v); err != nil {
				return fmt.Errorf("view %q: collMod validator failed: %w", v.Name(), err)
			}
		}
	}

	if specs := v.IndexSpecs(); len(specs) > 0 {
		if err := applyIndexes(ctx, m, v.Name(), specs, forceRebuild); err != nil {
			return err
		}
	}
	return nil
}

// lookupCollection runs listCollections({name}) and returns either the
// observed options document (when present) or (false, nil) when absent.
// Mongo's listCollections returns the canonical "options" sub-document
// the apply step compares against the declared spec.
func lookupCollection(ctx context.Context, m *MongoDB, name string) (bool, bson.M, error) {
	cur, err := m.db.ListCollections(ctx, bson.M{"name": name})
	if err != nil {
		return false, nil, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		return false, nil, cur.Err()
	}
	var info bson.M
	if err := cur.Decode(&info); err != nil {
		return false, nil, err
	}
	return true, info, nil
}

// createCollectionWithSpec issues a single createCollection with the
// declared collation / capped / time-series / validator options. Mongo's
// API accepts all four in one call — no need for follow-up collMod when
// the collection is born from this path.
func createCollectionWithSpec(ctx context.Context, m *MongoDB, v *ViewDefinition) error {
	opts := buildCreateCollectionOptions(v)
	return m.db.CreateCollection(ctx, v.Name(), opts)
}

// buildCreateCollectionOptions assembles the driver-native options
// builder from the declarative spec. Split out so the assembly logic is
// unit-testable without an active Mongo connection.
func buildCreateCollectionOptions(v *ViewDefinition) *options.CreateCollectionOptionsBuilder {
	opts := options.CreateCollection()
	if c := v.CollectionCollation(); c != nil {
		opts.SetCollation(c.DriverCollation())
	}
	if c := v.CappedDeclaration(); c != nil {
		opts.SetCapped(true).SetSizeInBytes(c.SizeBytes)
		if c.MaxDocs > 0 {
			opts.SetMaxDocuments(c.MaxDocs)
		}
	}
	if ts := v.TimeSeriesDeclaration(); ts != nil {
		tso := options.TimeSeries().SetTimeField(ts.TimeField)
		if ts.MetaField != "" {
			tso.SetMetaField(ts.MetaField)
		}
		if ts.Granularity != "" {
			tso.SetGranularity(ts.Granularity)
		}
		opts.SetTimeSeriesOptions(tso)
	}
	if js := v.SchemaSpec(); js != nil {
		opts.SetValidator(bson.M{"$jsonSchema": js.Schema})
		if js.ValidationLevel != "" {
			opts.SetValidationLevel(js.ValidationLevel)
		}
		if js.ValidationAction != "" {
			opts.SetValidationAction(js.ValidationAction)
		}
	}
	return opts
}

// applyValidator runs collMod to set / update the $jsonSchema validator
// on an existing collection. collMod is idempotent: running it twice
// with the same payload is a no-op as far as observable state goes.
func applyValidator(ctx context.Context, m *MongoDB, v *ViewDefinition) error {
	cmd := buildValidatorCommand(v)
	return m.db.RunCommand(ctx, cmd).Err()
}

// buildValidatorCommand assembles the collMod payload from the
// declarative spec. Returns a bson.D (order matters for collMod —
// `collMod` must come first as the command name).
func buildValidatorCommand(v *ViewDefinition) bson.D {
	js := v.SchemaSpec()
	cmd := bson.D{
		{Key: "collMod", Value: v.Name()},
		{Key: "validator", Value: bson.M{"$jsonSchema": js.Schema}},
	}
	if js.ValidationLevel != "" {
		cmd = append(cmd, bson.E{Key: "validationLevel", Value: js.ValidationLevel})
	}
	if js.ValidationAction != "" {
		cmd = append(cmd, bson.E{Key: "validationAction", Value: js.ValidationAction})
	}
	return cmd
}

// applyIndexes ensures every declared index exists on the collection.
// Strategy is per-spec CreateOne so the apply step can identify which
// index conflicted and recover individually under FORCE_REBUILD. The
// driver absorbs identical specs as no-ops (returns the existing name),
// so the steady-state cost is one round-trip per declared index.
func applyIndexes(ctx context.Context, m *MongoDB, collection string, specs []*IndexSpec, forceRebuild bool) error {
	col := m.db.Collection(collection)
	for _, s := range specs {
		model := s.IndexModel()
		_, err := col.Indexes().CreateOne(ctx, model)
		if err == nil {
			continue
		}
		if !isIndexConflict(err) {
			return fmt.Errorf("view %q: createIndex failed: %w", collection, err)
		}
		// Divergence path. Without the opt-in, abort loud.
		if !forceRebuild {
			return fmt.Errorf("view %q: index %q diverges from declared spec; set %s=true to drop and recreate (driver error: %v)",
				collection, deriveIndexName(s), EnvForceRebuild, err)
		}
		// FORCE_REBUILD: drop the conflicting index and retry.
		idxName := deriveIndexName(s)
		if dropErr := col.Indexes().DropOne(ctx, idxName); dropErr != nil {
			return fmt.Errorf("view %q: force-rebuild drop of index %q failed: %w (original conflict: %v)",
				collection, idxName, dropErr, err)
		}
		slog.WarnContext(ctx, "mongo.apply.index.dropped",
			slog.String("collection", collection),
			slog.String("index", idxName),
			slog.String("reason", "force-rebuild"))
		if _, err := col.Indexes().CreateOne(ctx, model); err != nil {
			return fmt.Errorf("view %q: force-rebuild recreate of index %q failed: %w",
				collection, idxName, err)
		}
		slog.InfoContext(ctx, "mongo.apply.index.rebuilt",
			slog.String("collection", collection),
			slog.String("index", idxName))
	}
	return nil
}

// isIndexConflict reports whether err is one of Mongo's "this index
// already exists with a different spec" command errors. Anything else
// is propagated unchanged.
func isIndexConflict(err error) bool {
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Code == mongoErrIndexOptionsConflict ||
			cmdErr.Code == mongoErrIndexKeySpecsConflict
	}
	return false
}

// deriveIndexName mirrors Mongo's auto-derivation rule
// (field_dir_field_dir, with "1"/"-1"/"text"/"2d"/"2dsphere"/"hashed"
// as direction tokens) so the apply step can name the index it just
// failed to create. An explicit IndexSpec.Name(...) takes priority —
// the framework honors the consumer's choice when set.
//
// The rule replicated here matches the driver's default naming function
// (see https://www.mongodb.com/docs/manual/indexes/#index-names) so a
// CreateOne that just succeeded against a steady-state cluster yields
// an index whose name equals deriveIndexName(s).
func deriveIndexName(s *IndexSpec) string {
	if s.name != "" {
		return s.name
	}
	parts := make([]string, 0, len(s.Keys))
	for _, k := range s.Keys {
		token := "1"
		switch k.Order {
		case IndexOrderDesc:
			token = "-1"
		case IndexOrderText:
			token = "text"
		case IndexOrderGeo2D:
			token = "2d"
		case IndexOrderGeo2DSph:
			token = "2dsphere"
		case IndexOrderHashed:
			token = "hashed"
		}
		parts = append(parts, k.Field+"_"+token)
	}
	return strings.Join(parts, "_")
}

// assertCollectionMatches verifies the observed listCollections options
// document satisfies the declared spec. Strict semantic: any divergence
// returns an error naming the view and the offending option. The apply
// step never auto-drops the collection — the operator owns the migration
// path on this branch (immutable Mongo options).
func assertCollectionMatches(v *ViewDefinition, observed bson.M) error {
	opts, _ := observed["options"].(bson.M)
	if opts == nil {
		opts = bson.M{}
	}
	if diag := collationDivergence(v.CollectionCollation(), opts); diag != "" {
		return fmt.Errorf("view %q: %s — collation is immutable on existing collections", v.Name(), diag)
	}
	if diag := cappedDivergence(v.CappedDeclaration(), opts); diag != "" {
		return fmt.Errorf("view %q: %s — capped options are immutable on existing collections", v.Name(), diag)
	}
	if diag := timeSeriesDivergence(v.TimeSeriesDeclaration(), opts); diag != "" {
		return fmt.Errorf("view %q: %s — time-series options are immutable on existing collections", v.Name(), diag)
	}
	return nil
}

// collationDivergence compares the declared CollationSpec against the
// observed collation sub-document. Empty string means "match"; a
// non-empty string is the human-readable diagnostic to embed in the
// caller's error.
//
// Comparison is forward-only: declared fields are compared against
// observed; observed fields the consumer did not declare (Mongo
// defaults, version) are ignored. This is the canonical "consumer
// declared X; X is what we honor" semantic — the framework does not
// assume the consumer wanted Mongo's default values they never
// pronounced.
func collationDivergence(decl *CollationSpec, opts bson.M) string {
	observed, hasObs := opts["collation"].(bson.M)
	if decl == nil && !hasObs {
		return ""
	}
	if decl == nil && hasObs {
		return "collation present on collection but absent from declaration"
	}
	if decl != nil && !hasObs {
		return "collation declared but absent on existing collection"
	}
	if locale, _ := observed["locale"].(string); locale != decl.Locale {
		return fmt.Sprintf("collation locale divergence (declared=%q, observed=%q)", decl.Locale, locale)
	}
	if decl.Strength != 0 {
		if got := readInt32(observed["strength"]); got != int32(decl.Strength) {
			return fmt.Sprintf("collation strength divergence (declared=%d, observed=%d)", decl.Strength, got)
		}
	}
	if decl.NumericOrdering {
		if got, _ := observed["numericOrdering"].(bool); !got {
			return "collation numericOrdering divergence (declared=true, observed=false)"
		}
	}
	if decl.Alternate != "" {
		if got, _ := observed["alternate"].(string); got != decl.Alternate {
			return fmt.Sprintf("collation alternate divergence (declared=%q, observed=%q)", decl.Alternate, got)
		}
	}
	return ""
}

// cappedDivergence compares the declared CappedSpec against the
// observed options. capped=true with mismatching size or max is
// reported; capped=false declared while observed=true (or vice versa)
// is reported.
func cappedDivergence(decl *CappedSpec, opts bson.M) string {
	obsCapped, _ := opts["capped"].(bool)
	if decl == nil && !obsCapped {
		return ""
	}
	if decl == nil && obsCapped {
		return "collection is capped but declaration is not"
	}
	if decl != nil && !obsCapped {
		return "Capped declared but collection is not capped"
	}
	if obsSize := readInt64(opts["size"]); obsSize != decl.SizeBytes {
		return fmt.Sprintf("capped SizeBytes divergence (declared=%d, observed=%d)", decl.SizeBytes, obsSize)
	}
	if decl.MaxDocs > 0 {
		if obsMax := readInt64(opts["max"]); obsMax != decl.MaxDocs {
			return fmt.Sprintf("capped MaxDocs divergence (declared=%d, observed=%d)", decl.MaxDocs, obsMax)
		}
	}
	return ""
}

// timeSeriesDivergence compares the declared TimeSeriesSpec against the
// observed timeseries sub-document.
func timeSeriesDivergence(decl *TimeSeriesSpec, opts bson.M) string {
	observed, hasObs := opts["timeseries"].(bson.M)
	if decl == nil && !hasObs {
		return ""
	}
	if decl == nil && hasObs {
		return "collection is time-series but declaration is not"
	}
	if decl != nil && !hasObs {
		return "TimeSeries declared but collection is not a time-series collection"
	}
	if got, _ := observed["timeField"].(string); got != decl.TimeField {
		return fmt.Sprintf("time-series TimeField divergence (declared=%q, observed=%q)", decl.TimeField, got)
	}
	if decl.MetaField != "" {
		if got, _ := observed["metaField"].(string); got != decl.MetaField {
			return fmt.Sprintf("time-series MetaField divergence (declared=%q, observed=%q)", decl.MetaField, got)
		}
	}
	if decl.Granularity != "" {
		if got, _ := observed["granularity"].(string); got != decl.Granularity {
			return fmt.Sprintf("time-series Granularity divergence (declared=%q, observed=%q)", decl.Granularity, got)
		}
	}
	return ""
}

// readInt32 / readInt64 normalize the bson number variants the driver
// may produce when decoding listCollections options (int32 / int64 /
// untyped int depending on the wire encoding). Returns 0 when the input
// is nil or a non-numeric type — callers always pair this with an
// explicit "declared > 0" guard so the zero return cannot be confused
// with a missing field.
func readInt32(v any) int32 {
	switch x := v.(type) {
	case int32:
		return x
	case int64:
		return int32(x)
	case int:
		return int32(x)
	case float64:
		return int32(x)
	}
	return 0
}

func readInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

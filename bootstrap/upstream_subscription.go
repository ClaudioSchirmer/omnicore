package bootstrap

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// UpstreamSubscription declares one Kafka topic produced by an upstream
// service A whose payload service B wants to materialize into a local Mongo
// collection. After every successful local upsert, every B view that embeds
// the collection via an external query.JoinUpstream is recomposed transparently by the
// framework — B writes no code per upstream change and never reads from A's
// database or HTTP surface on the request path.
//
// One subscription = one upstream entity type. Multiple subscriptions can
// coexist in a single service (e.g. `users.events` + `products.events` on
// an `orders` service), each running its own consumer group, its own worker
// pool, and its own GDPR policy.
//
// Defaults applied at boot when the field is left zero:
//
//   - ConsumerGroup:    "<service>-upstream-<topic>"
//   - Workers:          1
//   - Fields:           nil → full payload kept
//   - DeleteOnArchive:  false → ARCHIVED upserts a doc with deleted_at set
//   - StartFrom:        StartFromLatest
//   - OnUpstreamDelete: UpstreamDeleteCascade
//
// See tasks/mongo_cross_service_composition_final.md §4.1.
type UpstreamSubscription struct {
	// Topic is the Kafka topic produced by A (Debezium Outbox Event
	// Router shape — see infra/sync.go's extractEvent). Mandatory.
	Topic string `yaml:"topic"`

	// Collection is the local Mongo collection name where B
	// materializes the upstream-projected docs. Boot guard §8.2
	// rejects a collision with any local ViewDefinition.Name() AND a
	// duplicate Collection across subscriptions in the same Wiring.
	// Mandatory.
	Collection string `yaml:"collection"`

	// ConsumerGroup is the Kafka consumer group ID. Replicas of B share
	// the same group so each partition is processed once. Empty value
	// defaults to "<service>-upstream-<topic>" at boot.
	ConsumerGroup string `yaml:"consumerGroup"`

	// Workers is the per-subscription worker pool size. Aggregate IDs
	// route to a per-worker channel via FNV-1a hash so per-aggregate
	// ordering is preserved while different aggregates parallelize.
	// 0 or unset defaults to 1.
	Workers int `yaml:"workers"`

	// Fields is an allowlist of upstream payload fields that survive
	// in the local doc. Nil/empty keeps the full payload — appropriate
	// for low-cardinality entities. Declaring an explicit set is the
	// supported way to limit the local copy's data classification.
	Fields []string `yaml:"fields"`

	// DeleteOnArchive mirrors ViewDefinition.DeleteOnArchive: when
	// true, ARCHIVED events remove the local doc; when false (default),
	// the local doc survives with deleted_at populated. The semantics
	// stay symmetric with how B's own views handle archive.
	DeleteOnArchive bool `yaml:"deleteOnArchive"`

	// StartFrom controls the Kafka offset start position when the
	// consumer group has never committed. Default StartFromLatest. See
	// §7.4 for the operational shape (bootstrap a new B against an
	// existing A via the omnicore-admin replay CLI, not via earliest).
	StartFrom StartFromMode `yaml:"startFrom"`

	// OnUpstreamDelete dispatches GDPR / right-to-be-forgotten on a
	// DELETED event from A. Default UpstreamDeleteCascade. See §9 for
	// the three policies' downstream recompose semantics.
	OnUpstreamDelete UpstreamDeletePolicy `yaml:"onUpstreamDelete"`

	// AnonymizeFields is the explicit set of fields blanked when
	// OnUpstreamDelete is UpstreamDeleteAnonymize. Required (boot guard
	// §8.4 aborts on empty under the anonymize policy). Empty under
	// cascade or keep is fine — the policy does not consume it.
	AnonymizeFields []string `yaml:"anonymizeFields"`

	// AcknowledgeOffsetReset is the operator opt-in required when
	// StartFrom is "offset:<N>" under a non-dev profile. Guards against
	// accidental replay storms in production. Ignored under dev.
	AcknowledgeOffsetReset bool `yaml:"acknowledgeOffsetReset"`
}

// knownUpstreamSubscriptionKeys is the strict allowlist of yaml keys under
// each entry of upstreamSubscriptions:. Anything else aborts the boot —
// catches typos like "anonimizeFields" or removed/renamed fields instead of
// silently ignoring them.
var knownUpstreamSubscriptionKeys = map[string]bool{
	"topic":                  true,
	"collection":             true,
	"consumerGroup":          true,
	"workers":                true,
	"fields":                 true,
	"deleteOnArchive":        true,
	"startFrom":              true,
	"onUpstreamDelete":       true,
	"anonymizeFields":        true,
	"acknowledgeOffsetReset": true,
}

// UnmarshalYAML decodes a subscription entry while rejecting unknown keys.
// Mirrors the strict-yaml-decoding pattern used by MongoRebuildConfig: the
// allowlist runs before the field population so typos surface at boot.
func (s *UpstreamSubscription) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("upstreamSubscription: expected mapping, got %v", value.Kind)
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		if !knownUpstreamSubscriptionKeys[keyNode.Value] {
			return fmt.Errorf(
				"upstreamSubscription: unknown field %q "+
					"(allowed: topic, collection, consumerGroup, workers, filter, "+
					"deleteOnArchive, startFrom, onUpstreamDelete, anonymizeFields, "+
					"acknowledgeOffsetReset)",
				keyNode.Value,
			)
		}
	}
	// `type plain` avoids infinite recursion: aliasing strips the
	// custom UnmarshalYAML method so the default decoder runs the
	// actual field population.
	type plain UpstreamSubscription
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*s = UpstreamSubscription(p)
	return nil
}

// StartFromMode is the closed set of accepted values for
// UpstreamSubscription.StartFrom. The "offset:<N>" variant is a templated
// shape parsed at boot via startFromOffsetPattern; the constants here cover
// the symbolic values.
type StartFromMode string

const (
	// StartFromLatest tails from the head of the topic on first join.
	// Use in production when a new B was bootstrapped via the replay
	// CLI and now follows the live stream.
	StartFromLatest StartFromMode = "latest"

	// StartFromEarliest reads from offset 0 of the topic on first
	// join. Use only when A is brand new and retention covers the full
	// history — otherwise prefer the replay CLI to snapshot A's
	// current state.
	StartFromEarliest StartFromMode = "earliest"
)

// startFromOffsetPattern matches the templated form "offset:<N>" used by
// StartFrom. Operators set this for coordinated PITR recovery; non-dev
// profiles also require AcknowledgeOffsetReset: true on the same
// subscription (validated in §6.3).
var startFromOffsetPattern = regexp.MustCompile(`^offset:[0-9]+$`)

// UpstreamDeletePolicy is the closed set of right-to-be-forgotten
// propagation policies. See §9 for the downstream recompose semantics of
// each one.
type UpstreamDeletePolicy string

const (
	// UpstreamDeleteCascade deletes the local doc AND recomposes every
	// dependent view so the embed resolves to null. Default — the
	// conservative GDPR-aligned choice when B has no business reason
	// to retain a snapshot of the deleted entity.
	UpstreamDeleteCascade UpstreamDeletePolicy = "cascade"

	// UpstreamDeleteAnonymize sets the declared AnonymizeFields to
	// null on the local doc and recomposes every dependent view with
	// the blanked embed. Use when B's business history must reference
	// the entity (orders carry buyer_id for accounting) but identity
	// must be removed.
	UpstreamDeleteAnonymize UpstreamDeletePolicy = "anonymize"

	// UpstreamDeleteKeep is a no-op on the local collection and skips
	// recompose. Use ONLY when B has a legal basis to retain the data
	// independent of A's request (regulatory retention, audit logs
	// that legally outlast the upstream entity).
	UpstreamDeleteKeep UpstreamDeletePolicy = "keep"
)

// validUpstreamDeletePolicies pre-builds the set of accepted values for
// fast membership checks during validation.
var validUpstreamDeletePolicies = map[UpstreamDeletePolicy]bool{
	UpstreamDeleteCascade:   true,
	UpstreamDeleteAnonymize: true,
	UpstreamDeleteKeep:      true,
}

// validStartFromSymbols covers the two symbolic StartFrom values.
// "offset:<N>" is matched via startFromOffsetPattern instead.
var validStartFromSymbols = map[StartFromMode]bool{
	StartFromLatest:   true,
	StartFromEarliest: true,
}

// applyDefaults resolves the per-subscription defaults documented on
// UpstreamSubscription. Called from bootstrap.Run after the YAML decoder
// returns. Service name is supplied because the default ConsumerGroup
// shape depends on it.
func (s *UpstreamSubscription) applyDefaults(service string) {
	if s.ConsumerGroup == "" {
		s.ConsumerGroup = fmt.Sprintf("%s-upstream-%s", service, s.Topic)
	}
	if s.Workers < 1 {
		s.Workers = 1
	}
	if s.StartFrom == "" {
		s.StartFrom = StartFromLatest
	}
	if s.OnUpstreamDelete == "" {
		s.OnUpstreamDelete = UpstreamDeleteCascade
	}
}

// validateShape checks the per-subscription invariants that are local to
// the struct (enum membership, "offset:N" regex, AcknowledgeOffsetReset
// gate against non-dev profiles). Multi-subscription invariants
// (collection collision, view↔subscription matching) live in the boot
// guards run from bootstrap.Run.
//
// profile is the runtime APP_PROFILE; "offset:<N>" under any non-"dev"
// profile requires AcknowledgeOffsetReset: true.
func (s UpstreamSubscription) validateShape(profile string) error {
	if s.Topic == "" {
		return fmt.Errorf("upstreamSubscription: topic is required")
	}
	if s.Collection == "" {
		return fmt.Errorf("upstreamSubscription %q: collection is required", s.Topic)
	}
	if !validUpstreamDeletePolicies[s.OnUpstreamDelete] {
		return fmt.Errorf(
			"upstreamSubscription %q: onUpstreamDelete %q invalid (want %q | %q | %q)",
			s.Topic, s.OnUpstreamDelete,
			UpstreamDeleteCascade, UpstreamDeleteAnonymize, UpstreamDeleteKeep,
		)
	}
	if !validStartFromSymbols[s.StartFrom] {
		// Not a symbolic value — try the offset:N template.
		if !startFromOffsetPattern.MatchString(string(s.StartFrom)) {
			return fmt.Errorf(
				"upstreamSubscription %q: startFrom %q invalid (want %q | %q | %q)",
				s.Topic, s.StartFrom,
				StartFromLatest, StartFromEarliest, "offset:<N>",
			)
		}
		// offset:N — guard against accidental replay storms in
		// non-dev profiles unless the operator opted in.
		if profile != profileDev && !s.AcknowledgeOffsetReset {
			return fmt.Errorf(
				"upstreamSubscription %q: startFrom=%q under profile %q requires acknowledgeOffsetReset: true",
				s.Topic, s.StartFrom, profile,
			)
		}
	}
	if s.Workers < 0 {
		// Workers == 0 is accepted because applyDefaults normalizes
		// it to 1; explicitly negative is a typo and aborts.
		return fmt.Errorf("upstreamSubscription %q: workers %d invalid (must be > 0 when declared)", s.Topic, s.Workers)
	}
	return nil
}

// describeFilter is a small helper for diagnostic messages — returns
// "[a, b, c]" or "(empty)" so error strings stay readable.
func describeFilter(fields []string) string {
	if len(fields) == 0 {
		return "(empty)"
	}
	return "[" + strings.Join(fields, ", ") + "]"
}

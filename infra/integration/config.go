// Package integration is the cross-service async messaging surface — the
// canonical write-side async path. Producers Dispatch typed events into
// the local `integration_events` table (atomic with the data write when
// WithTx(tx) is supplied from a BeforeCommit hook closure); subscribers
// receive Kafka messages via Receivers attached to the framework's
// Registry, route each payload through the same pipeline.Handler[TCmd,
// TResult] HTTP routes consume, and dedup per consumer group via
// `omnicore_integration_processed`.
//
// Vocabulary mirrors the canonical pub/sub literature: `publishes`
// (events this service emits) + `subscribes` (events this service
// consumes) layered on the same `integration:` YAML block. Wire
// `event_type` strings live in YAML; Go code references YAML KEYS
// (`eventKey`, `sourceKey`) so a wire rename is a YAML edit, not a code
// sweep.
//
// The package consciously avoids domain coupling: domain events
// (intra-context, observability) and integration events (cross-context,
// infra primitive) stay separate concepts. The domain layer never
// imports this package; the application layer optionally calls
// fwintegration.Dispatch from inside a hook closure when atomicity with
// the data write is desired.
package integration

import (
	"fmt"
)

// Config is the in-process projection of the YAML `integration:` block.
// Bootstrap parses microservice.<profile>.yaml into this shape and calls
// Configure(...) before any feature mounts a receiver.
//
// The struct has both halves of the pub/sub surface co-resident so the
// operator-facing block is a single coherent unit; the framework reads
// `Publishes` from Dispatch and `Subscribes` from the receiver pool.
type Config struct {
	Publishes  PublishConfig             `yaml:"publishes"`
	Subscribes map[string]SubscribeEntry `yaml:"subscribes"`
	Defaults   SubscriberDefaults        `yaml:"defaults"`
}

// PublishConfig declares the events this service emits via Dispatch.
// Each entry under Events keys by `eventKey` — the same Go-side identifier
// passed to Dispatch. Renaming a wire `event_type` (schema migration
// `UserActivated` → `UserAccountActivated`) edits the YAML's `eventType`
// field; the Go-side eventKey stays stable.
type PublishConfig struct {
	Events map[string]PublishEvent `yaml:"events"`
}

// PublishEvent describes one emitted event. EventType is the wire
// identifier consumers match against the Kafka header; Aggregate is the
// aggregate-type slot for `integration_events.aggregate_type` (left
// empty for standalone events with no aggregate binding); Version is the
// integer the framework stamps on `event_version`.
type PublishEvent struct {
	EventType string `yaml:"eventType"`
	Aggregate string `yaml:"aggregate"`
	Version   int    `yaml:"version"`
}

// SubscribeEntry declares one upstream source — typically a sibling
// service's `<aggregate>.integration.events` topic. Events maps each
// `eventKey` consumed from this source to the wire `eventType` the
// producer emitted; the Receiver routes by exact match on the wire value.
// Workers, ConsumerGroup, and StartFrom override the corresponding
// defaults from SubscriberDefaults.
type SubscribeEntry struct {
	Topic         string                    `yaml:"topic"`
	Events        map[string]SubscribeEvent `yaml:"events"`
	Workers       int                       `yaml:"workers"`
	ConsumerGroup string                    `yaml:"consumerGroup"`
	StartFrom     string                    `yaml:"startFrom"`
}

// SubscribeEvent maps a Go-side eventKey to the wire eventType the
// producer emits. The shape stays minimal — additional knobs (per-event
// retry policy, per-event idempotency hint) would land here without
// breaking the YAML grammar.
type SubscribeEvent struct {
	EventType string `yaml:"eventType"`
}

// SubscriberDefaults seeds every SubscribeEntry that omits the matching
// field. ConsumerGroup defaults to `"<cfg.Service>-integration"` when
// empty, computed at Configure time from the service name passed in.
type SubscriberDefaults struct {
	ConsumerGroup string `yaml:"consumerGroup"`
	Workers       int    `yaml:"workers"`
	StartFrom     string `yaml:"startFrom"`
}

// LookupPublish returns the publish entry for the eventKey, or false when
// the key is not declared in YAML. Dispatch uses this for lazy
// validation: an unknown key surfaces as ErrIntegrationEventNotConfigured
// at the first call site rather than aborting boot — matches the posture
// httpclient takes for unknown service/endpoint references.
func (c *Config) LookupPublish(eventKey string) (PublishEvent, bool) {
	if c == nil {
		return PublishEvent{}, false
	}
	ev, ok := c.Publishes.Events[eventKey]
	if !ok {
		return PublishEvent{}, false
	}
	return ev, true
}

// LookupSubscribe resolves a (sourceKey, eventKey) pair into the wire
// metadata the receiver pipeline needs. Returns the source's Kafka topic,
// the event's wire eventType, and false when either coordinate is
// missing. Used by Registry.MountReceivers to eager-validate every
// registered receiver against YAML — boot aborts with a precise
// diagnostic when a receiver references a missing coordinate.
func (c *Config) LookupSubscribe(sourceKey, eventKey string) (topic, eventType string, ok bool) {
	if c == nil {
		return "", "", false
	}
	src, has := c.Subscribes[sourceKey]
	if !has {
		return "", "", false
	}
	ev, has := src.Events[eventKey]
	if !has {
		return "", "", false
	}
	return src.Topic, ev.EventType, true
}

// ApplyDefaults fills empty fields on each SubscribeEntry from the
// defaults block. serviceName is used to compute the consumer-group
// fallback `"<serviceName>-integration"` — bootstrap reads it from
// cfg.Service. Returns the modified Config so chained boot helpers can
// stay one-line.
func (c *Config) ApplyDefaults(serviceName string) {
	if c == nil {
		return
	}
	if c.Defaults.ConsumerGroup == "" {
		c.Defaults.ConsumerGroup = serviceName + "-integration"
	}
	if c.Defaults.Workers <= 0 {
		c.Defaults.Workers = 0 // signal to ConsumerPool to pick runtime.NumCPU()
	}
	if c.Defaults.StartFrom == "" {
		c.Defaults.StartFrom = "latest"
	}
	if c.Subscribes == nil {
		c.Subscribes = map[string]SubscribeEntry{}
	}
	for k, src := range c.Subscribes {
		if src.ConsumerGroup == "" {
			src.ConsumerGroup = c.Defaults.ConsumerGroup
		}
		if src.Workers <= 0 {
			src.Workers = c.Defaults.Workers
		}
		if src.StartFrom == "" {
			src.StartFrom = c.Defaults.StartFrom
		}
		c.Subscribes[k] = src
	}
}

// Validate enforces structural rules independent of the runtime profile.
// Called from bootstrap.LoadConfig right after the YAML decoder returns.
// Errors aggregate so the operator sees the whole list on one boot
// attempt — same posture httpclient.Validate adopts.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []string
	for k, ev := range c.Publishes.Events {
		if ev.EventType == "" {
			problems = append(problems, fmt.Sprintf("integration.publishes.events.%s.eventType is required", k))
		}
		if ev.Version < 0 {
			problems = append(problems, fmt.Sprintf("integration.publishes.events.%s.version must be >= 0 (got %d)", k, ev.Version))
		}
	}
	for srcKey, src := range c.Subscribes {
		if src.Topic == "" {
			problems = append(problems, fmt.Sprintf("integration.subscribes.%s.topic is required", srcKey))
		}
		if src.Workers < 0 {
			problems = append(problems, fmt.Sprintf("integration.subscribes.%s.workers must be >= 0 (got %d)", srcKey, src.Workers))
		}
		if !validStartFrom(src.StartFrom) {
			problems = append(problems, fmt.Sprintf("integration.subscribes.%s.startFrom %q is not one of earliest|latest", srcKey, src.StartFrom))
		}
		for evKey, ev := range src.Events {
			if ev.EventType == "" {
				problems = append(problems, fmt.Sprintf("integration.subscribes.%s.events.%s.eventType is required", srcKey, evKey))
			}
		}
	}
	if c.Defaults.Workers < 0 {
		problems = append(problems, fmt.Sprintf("integration.defaults.workers must be >= 0 (got %d)", c.Defaults.Workers))
	}
	if !validStartFrom(c.Defaults.StartFrom) {
		problems = append(problems, fmt.Sprintf("integration.defaults.startFrom %q is not one of earliest|latest", c.Defaults.StartFrom))
	}
	if len(problems) > 0 {
		return fmt.Errorf("integration config: %d problem(s):\n  - %s",
			len(problems), joinProblems(problems))
	}
	return nil
}

// validStartFrom reports whether s is an accepted startFrom symbol. Empty is
// accepted because ApplyDefaults fills it ("latest") and a fresh Validate may
// run before that; "earliest" and "latest" are the only values the
// ConsumerPool honors (every other value would silently resolve to "latest").
func validStartFrom(s string) bool {
	switch s {
	case "", "earliest", "latest":
		return true
	default:
		return false
	}
}

func joinProblems(p []string) string {
	out := p[0]
	for _, s := range p[1:] {
		out += "\n  - " + s
	}
	return out
}

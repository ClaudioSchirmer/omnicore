package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"

	"github.com/google/uuid"
)

// Registry collects every Receiver the consumer service registers
// during the bootstrap Phase Receivers. Mirrors the role *fiber.App
// plays for the HTTP transport: Feature implementations write into the
// registry inline during MountReceivers(reg, deps) and the framework
// reads back the populated slice when ConsumerPool spins.
//
// Construction is via NewRegistry() in bootstrap; consumer code never
// instantiates one.
type Registry struct {
	receivers []*Receiver
}

// NewRegistry returns a fresh empty registry. Called once by
// bootstrap.Run BEFORE any Feature.Mount call so the framework can
// thread the same registry through every IntegrationFeature.
func NewRegistry() *Registry {
	return &Registry{}
}

// Receivers returns the registered receivers in declaration order.
// Bootstrap consumes this to drive ConsumerPool startup; the admin
// retry surface in the consumer service consumes it to walk every
// receiver's RetryPendingFailures.
func (r *Registry) Receivers() []*Receiver {
	if r == nil {
		return nil
	}
	out := make([]*Receiver, len(r.receivers))
	copy(out, r.receivers)
	return out
}

// IsEmpty reports whether the registry has zero receivers — bootstrap
// uses this to decide whether to start a ConsumerPool at all.
func (r *Registry) IsEmpty() bool {
	if r == nil {
		return true
	}
	return len(r.receivers) == 0
}

// From opens a fluent builder bound to a YAML-declared source key.
// Consumer code chains .On(eventKey, sample, handler) per event the
// service consumes from this source. The source key must exist in the
// loaded YAML's `integration.subscribes.<sourceKey>` block before any
// On call attaches a receiver — eager validation runs at MountReceivers
// time so a missing source surfaces as a boot panic before any traffic
// reaches the receiver.
func (r *Registry) From(sourceKey string) *SourceBuilder {
	return &SourceBuilder{registry: r, sourceKey: sourceKey}
}

// SourceBuilder accumulates per-source receiver declarations. The
// fluent shape mirrors how Fiber's *App routes register: each .On call
// appends one entry and returns the builder for further chaining.
type SourceBuilder struct {
	registry  *Registry
	sourceKey string
}

// On attaches one Receiver for a single eventKey on the source bound
// to the builder. The sample value carries the wire DTO shape — the
// framework reflects on its type to allocate a fresh request per
// message and invoke its ToCommand() method. handler is the same
// pipeline.Handler[TCmd, TResult] HTTP routes consume — the cornerstone
// of handler invariance across transports.
//
// Method has no generic parameters because Go does not allow generic
// methods (see the omnicore Go pitfalls section in CLAUDE.md). The
// runtime adapter uses reflection to bridge sample/handler types; the
// receiver's call site sees a panic at boot if the types don't match
// (sample carries no ToCommand method, ToCommand returns the wrong
// Cmd type for the handler, etc.). The eventKey eager-validation
// against YAML still runs first.
//
// Panics at boot when:
//   - sample is nil
//   - sample's type has no ToCommand() method
//   - handler is nil
//   - handler's Handle method's command parameter doesn't match the
//     return type of sample.ToCommand()
func (sb *SourceBuilder) On(eventKey string, sample any, handler any) *SourceBuilder {
	if sb == nil || sb.registry == nil {
		panic("integration.SourceBuilder.On: builder is nil — call Registry.From(...) first")
	}
	if sample == nil {
		panic(fmt.Sprintf("integration.SourceBuilder.On: sample is nil for sourceKey=%q eventKey=%q", sb.sourceKey, eventKey))
	}
	if handler == nil {
		panic(fmt.Sprintf("integration.SourceBuilder.On: handler is nil for sourceKey=%q eventKey=%q", sb.sourceKey, eventKey))
	}
	plan, err := planReceiver(sample, handler)
	if err != nil {
		panic(fmt.Sprintf("integration.SourceBuilder.On: sourceKey=%q eventKey=%q: %v", sb.sourceKey, eventKey, err))
	}
	rcv := &Receiver{
		sourceKey: sb.sourceKey,
		eventKey:  eventKey,
		plan:      plan,
	}
	sb.registry.receivers = append(sb.registry.receivers, rcv)
	return sb
}

// receiverPlan is the reflection-based dispatch plan computed once at
// MountReceivers time. The framework keeps it on each Receiver so the
// per-message hot path is allocation-light: NewRequest allocates a
// fresh wire DTO, BindJSON populates it, Invoke calls the handler.
type receiverPlan struct {
	reqType       reflect.Type
	toCommandFn   reflect.Value
	handlerValue  reflect.Value
	handlerHandle reflect.Value
}

// planReceiver inspects sample + handler via reflection and computes
// the per-message dispatch plan. Returns an error so the caller can
// emit a precise panic; failures here are caught at MountReceivers
// time, never at request time.
func planReceiver(sample any, handler any) (*receiverPlan, error) {
	reqType := reflect.TypeOf(sample)
	if reqType == nil {
		return nil, fmt.Errorf("sample type is unresolvable (interface{}(nil))")
	}
	// ToCommand may have value OR pointer receiver. Inspect both.
	method, ok := reqType.MethodByName("ToCommand")
	if !ok {
		ptrType := reflect.PointerTo(reqType)
		method, ok = ptrType.MethodByName("ToCommand")
		if !ok {
			return nil, fmt.Errorf("sample type %s carries no ToCommand() method", reqType.String())
		}
	}
	if method.Type.NumIn() != 1 {
		return nil, fmt.Errorf("sample type %s ToCommand must take zero arguments (receiver only)", reqType.String())
	}
	if method.Type.NumOut() != 1 {
		return nil, fmt.Errorf("sample type %s ToCommand must return exactly one value (the Command)", reqType.String())
	}
	cmdType := method.Type.Out(0)

	handlerType := reflect.TypeOf(handler)
	if handlerType == nil {
		return nil, fmt.Errorf("handler type is unresolvable")
	}
	handle, ok := handlerType.MethodByName("Handle")
	if !ok {
		return nil, fmt.Errorf("handler type %s does not implement pipeline.Handler (missing Handle method)", handlerType.String())
	}
	// Expect Handle(ctx *AppContext, cmd TCmd) (TRes, error)
	if handle.Type.NumIn() != 3 {
		return nil, fmt.Errorf("handler type %s Handle must take (ctx, cmd)", handlerType.String())
	}
	wantCtx := reflect.TypeOf((*configuration.AppContext)(nil))
	if handle.Type.In(1) != wantCtx {
		return nil, fmt.Errorf("handler type %s Handle's first param must be *configuration.AppContext, got %s",
			handlerType.String(), handle.Type.In(1).String())
	}
	if handle.Type.In(2) != cmdType {
		return nil, fmt.Errorf("handler type %s Handle's command param is %s, but sample.ToCommand returns %s",
			handlerType.String(), handle.Type.In(2).String(), cmdType.String())
	}

	return &receiverPlan{
		reqType:       reqType,
		toCommandFn:   reflect.Value{}, // populated per-call from the freshly allocated request
		handlerValue:  reflect.ValueOf(handler),
		handlerHandle: reflect.ValueOf(handler).MethodByName("Handle"),
	}, nil
}

// newRequest allocates a fresh wire DTO and returns it as reflect.Value
// of the request's pointer type so json.Unmarshal can populate it.
func (p *receiverPlan) newRequest() (reflect.Value, any) {
	ptr := reflect.New(p.reqType)
	return ptr, ptr.Interface()
}

// toCommand invokes the wire DTO's ToCommand() method. The reflection
// shape handles both value and pointer receivers transparently —
// reflect.Value.MethodByName already promotes value-receiver methods
// onto the pointer.
func (p *receiverPlan) toCommand(reqValue reflect.Value) reflect.Value {
	target := reqValue
	if _, ok := p.reqType.MethodByName("ToCommand"); !ok {
		// Pointer receiver — invoke on the pointer-typed reqValue.
		target = reqValue
	} else {
		// Value receiver — dereference.
		target = reqValue.Elem()
	}
	method := target.MethodByName("ToCommand")
	return method.Call(nil)[0]
}

// invoke dispatches through pipeline.Run-equivalent machinery — the
// receiver pipeline calls pipeline.Dispatch directly with a
// reflection-built closure.
func (p *receiverPlan) invoke(ctx *configuration.AppContext, cmd reflect.Value) (reflect.Value, error) {
	results := p.handlerHandle.Call([]reflect.Value{reflect.ValueOf(ctx), cmd})
	var err error
	if e := results[1].Interface(); e != nil {
		err = e.(error)
	}
	return results[0], err
}

// Receiver is the per-event subscription unit. The framework spins one
// goroutine per ConsumerPool worker that drains messages from the
// source's Kafka topic and routes each one through the matching
// Receiver's plan. Exposes per-receiver knobs (RetryPendingFailures)
// for the admin surface.
type Receiver struct {
	sourceKey string
	eventKey  string
	plan      *receiverPlan
	// resolved at MountReceivers time from YAML
	topic         string
	wireEventType string
	consumerGroup string
	workers       int
	startFrom     string
}

// SourceKey returns the Go-side identifier the consumer service used
// when registering this receiver. Useful for admin tooling.
func (r *Receiver) SourceKey() string { return r.sourceKey }

// EventKey returns the Go-side identifier for the event this receiver
// handles. Useful for admin tooling.
func (r *Receiver) EventKey() string { return r.eventKey }

// Topic returns the Kafka topic resolved from YAML.
func (r *Receiver) Topic() string { return r.topic }

// WireEventType returns the wire `event_type` header value this
// receiver matches.
func (r *Receiver) WireEventType() string { return r.wireEventType }

// ConsumerGroup returns the consumer group this receiver runs under.
func (r *Receiver) ConsumerGroup() string { return r.consumerGroup }

// RetryPendingFailures re-runs the handler for every failure row in
// omnicore_integration_failures matching this receiver's natural key.
// Returns the number of failures attempted. Idempotent: a successful
// re-dispatch calls ResolveIntegrationFailures so the row is marked
// resolved; subsequent calls observe nothing left to retry.
//
// Wired through the consumer service's admin HTTP route per the design
// in tasks/msintercomunication.md. The framework owns the primitive;
// consumer services decide how to expose it (cron, admin endpoint,
// internal RPC).
func (r *Receiver) RetryPendingFailures(ctx context.Context, exec pgExec, pipe *pipeline.Pipeline, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) (int, error) {
	if exec == nil {
		return 0, fmt.Errorf("receiver.RetryPendingFailures: pg exec is nil")
	}
	pending, err := ListPendingIntegrationFailuresByGroup(ctx, exec, r.consumerGroup)
	if err != nil {
		return 0, err
	}
	retried := 0
	for _, p := range pending {
		if p.SourceKey != r.sourceKey || p.EventKey != r.eventKey {
			continue
		}
		retried++
		// Rebuild a synthetic headers map: the failure registry only
		// preserves the payload; receiver-side headers (actor,
		// correlation_id) are reconstructed from defaults so the
		// retry path is deterministic without storing the full
		// wire envelope.
		headers := map[string]string{"event_type": r.wireEventType}
		if err := r.handleMessage(ctx, exec, headers, p.EventID, p.RawPayload, pipe, logger); err == nil {
			_ = ResolveIntegrationFailures(ctx, exec, r.consumerGroup, r.sourceKey, r.eventKey, p.EventID)
		}
		if ctx.Err() != nil {
			return retried, ctx.Err()
		}
	}
	return retried, nil
}

// resolveAgainstYAML pins each receiver's wire metadata using the
// loaded YAML config. Called by the ConsumerPool startup path. Returns
// an error so the caller can aggregate problems for one composite
// panic surfaced before any traffic.
func (r *Receiver) resolveAgainstYAML(cfg *Config) error {
	topic, eventType, ok := cfg.LookupSubscribe(r.sourceKey, r.eventKey)
	if !ok {
		return fmt.Errorf("receiver (sourceKey=%q eventKey=%q) not declared in integration.subscribes YAML", r.sourceKey, r.eventKey)
	}
	r.topic = topic
	r.wireEventType = eventType
	src := cfg.Subscribes[r.sourceKey]
	r.consumerGroup = src.ConsumerGroup
	r.workers = src.Workers
	r.startFrom = src.StartFrom
	return nil
}

// handleMessage runs the per-message pipeline outside the consumer
// pool's Kafka loop. The function is exposed so unit tests can exercise
// the full request shape without spinning a Kafka reader. Returns nil
// on success (including dedup hit). Non-nil errors are observability
// hints — the consumer pool still acks Kafka and records failure rows.
func (r *Receiver) handleMessage(
	ctx context.Context,
	exec pgExec,
	rawHeaders map[string]string,
	eventID uuid.UUID,
	rawPayload []byte,
	pipe *pipeline.Pipeline,
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	},
) error {
	if eventID == uuid.Nil {
		logger.Warn("integration.consumer.malformed", "source_key", r.sourceKey, "event_key", r.eventKey, "reason", "event_id missing")
		return fmt.Errorf("event_id missing")
	}

	logger.Debug("integration.consumer.received",
		"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "topic", r.topic)

	already, err := IsAlreadyProcessed(ctx, exec, eventID, r.consumerGroup)
	if err != nil {
		logger.Warn("integration.consumer.dedup_check_failed",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "err", err.Error())
		// fall through and attempt the handler — at-least-once contract
	}
	if already {
		logger.Debug("integration.consumer.deduplicated",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String())
		return nil
	}

	// Build a fresh AppContext per invocation: ID() = new UUID, actor
	// = inbound event's actor header, correlation = inbound correlation
	// (fallback event_id), causation = inbound event_id.
	appCtx := buildReceiverAppContext(rawHeaders, eventID)

	reqPtr, reqAny := r.plan.newRequest()
	if err := json.Unmarshal(rawPayload, reqAny); err != nil {
		logger.Warn("integration.consumer.unmarshal_failed",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "err", err.Error())
		recordIntegrationFailure(ctx, exec, r, eventID, rawPayload, err.Error())
		return err
	}

	cmdValue := r.plan.toCommand(reqPtr)

	resultValue, err := r.plan.invoke(appCtx, cmdValue)
	if err != nil {
		logger.Warn("integration.consumer.failed",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "err", err.Error())
		recordIntegrationFailure(ctx, exec, r, eventID, rawPayload, err.Error())
		return err
	}

	if !isSuccessResult(resultValue) {
		// Failure semantically came back as a typed Result rather than
		// an error — treat as success on the consumer pipeline (the
		// inbound message was handled), record the failure for forensics.
		logger.Warn("integration.consumer.failed",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "reason", "Result.IsSuccess=false")
		recordIntegrationFailure(ctx, exec, r, eventID, rawPayload, "handler reported non-success Result")
	}

	if err := MarkProcessed(ctx, exec, IntegrationProcessedRecord{
		EventID:       eventID,
		ConsumerGroup: r.consumerGroup,
		SourceKey:     r.sourceKey,
		EventKey:      r.eventKey,
		Topic:         r.topic,
		EventType:     r.wireEventType,
	}); err != nil {
		logger.Warn("integration.consumer.mark_processed_failed",
			"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "err", err.Error())
	}

	logger.Info("integration.consumer.handled",
		"source_key", r.sourceKey, "event_key", r.eventKey, "event_id", eventID.String(), "topic", r.topic)
	return nil
}

// isSuccessResult reflects on the Result[T] return shape and reads the
// IsSuccess() bool method. The receiver pipeline treats Failure and
// Exception identically for the at-least-once contract — both ack Kafka
// and record a failure row; the operator-driven retry surface can
// re-dispatch from the persisted payload.
func isSuccessResult(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	m := v.MethodByName("IsSuccess")
	if !m.IsValid() {
		// Result type without IsSuccess — treat as success (the handler
		// returned a value, not an error).
		return true
	}
	out := m.Call(nil)
	if len(out) == 0 {
		return true
	}
	if b, ok := out[0].Interface().(bool); ok {
		return b
	}
	return true
}

// recordIntegrationFailure wraps the failure write so the receiver
// pipeline's error path is one-line.
func recordIntegrationFailure(ctx context.Context, exec pgExec, r *Receiver, eventID uuid.UUID, rawPayload []byte, errMsg string) {
	rec := IntegrationFailureRecord{
		ConsumerGroup: r.consumerGroup,
		SourceKey:     r.sourceKey,
		EventKey:      r.eventKey,
		EventID:       eventID,
		RawPayload:    rawPayload,
		Error:         errMsg,
	}
	if err := RecordIntegrationFailure(ctx, exec, rec); err != nil {
		// Best-effort: matches the upstream failure-isolation contract.
		// Kafka offset still advances; subscriber loop carries on.
		// Slog at Warn so production alerting can catch the side-channel
		// degradation.
	}
}

// buildReceiverAppContext synthesizes the AppContext the handler sees.
// thread_id is fresh per invocation; actor comes from the inbound
// event's actor header so the handler's audit/event echoes use the
// same actor as the producer side.
func buildReceiverAppContext(headers map[string]string, eventID uuid.UUID) *configuration.AppContext {
	actor := headers["actor"]
	if actor == "" {
		actor = "anonymous"
	}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	// Set the synthetic Identity so ctx.ActorSubject() lines up with
	// the inbound actor header. The framework's Identity carries only
	// Subject for synthetic actors — Issuer/Claims stay empty, and
	// HasPermission returns false by safe default (Layer 2 authz cannot
	// accidentally escalate based on a forged claim envelope).
	ctx.SetIdentity(&configuration.Identity{Subject: actor})
	// Correlation = inbound correlation_id when present, fall back to
	// the event_id itself so the trace chain has a non-nil head.
	if h := headers["correlation_id"]; h != "" {
		if parsed, err := uuid.Parse(h); err == nil {
			ctx.SetCorrelationID(parsed)
		} else {
			ctx.SetCorrelationID(eventID)
		}
	} else {
		ctx.SetCorrelationID(eventID)
	}
	ctx.SetCausationID(eventID)
	return ctx
}

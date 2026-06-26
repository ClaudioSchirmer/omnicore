package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

func enabled(mut func(*Config)) Config {
	c := Config{
		Enabled:     true,
		Exporter:    ExporterNone,
		Endpoint:    "localhost:4317",
		Sampler:     SamplerParentBasedTraceRatio,
		Ratio:       0.1,
		ServiceName: "svc",
		Instrument:  map[Subsystem]bool{SubPgx: true},
	}
	if mut != nil {
		mut(&c)
	}
	return c
}

func TestConfigValidate(t *testing.T) {
	t.Run("disabled skips validation", func(t *testing.T) {
		c := Config{Enabled: false, Exporter: "garbage", Sampler: "garbage", Ratio: 9}
		if err := c.Validate(); err != nil {
			t.Fatalf("disabled config should validate, got %v", err)
		}
	})
	t.Run("ok", func(t *testing.T) {
		if err := enabled(nil).Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	cases := map[string]func(*Config){
		"bad exporter": func(c *Config) { c.Exporter = "weird" },
		"bad sampler":  func(c *Config) { c.Sampler = "weird" },
		"ratio high":   func(c *Config) { c.Ratio = 1.5 },
		"ratio low":    func(c *Config) { c.Ratio = -0.1 },
		"otlp no addr": func(c *Config) { c.Exporter = ExporterOTLP; c.Endpoint = "  " },
		"no service":   func(c *Config) { c.ServiceName = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			if err := enabled(mut).Validate(); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestInstruments(t *testing.T) {
	c := enabled(func(c *Config) { c.Instrument = map[Subsystem]bool{SubPgx: true, SubMongo: false} })
	if !c.Instruments(SubPgx) {
		t.Fatal("pgx should be instrumented")
	}
	if c.Instruments(SubMongo) {
		t.Fatal("mongo explicitly off")
	}
	if c.Instruments(SubKafka) {
		t.Fatal("absent subsystem off")
	}
	off := enabled(func(c *Config) { c.Enabled = false })
	if off.Instruments(SubPgx) {
		t.Fatal("disabled tracing instruments nothing")
	}
}

func TestSetupDisabledIsInert(t *testing.T) {
	p, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("disabled setup: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("inert shutdown: %v", err)
	}
	// nil receiver is also safe.
	var nilP *Provider
	if err := nilP.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil shutdown: %v", err)
	}
}

func TestSetupEnabledNoExportAndShutdown(t *testing.T) {
	p, err := Setup(context.Background(), enabled(func(c *Config) { c.Exporter = ExporterNone }))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if p.shutdown == nil {
		t.Fatal("enabled provider should carry a shutdown func")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetupOTLPExporterLazyConnect(t *testing.T) {
	// The OTLP/gRPC exporter dials lazily, so construction succeeds with no
	// collector listening — boot is never blocked by a down collector.
	p, err := Setup(context.Background(), enabled(func(c *Config) {
		c.Exporter = ExporterOTLP
		c.Endpoint = "localhost:4317"
		c.Sampler = SamplerAlwaysOn
	}))
	if err != nil {
		t.Fatalf("otlp setup: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetupOTLPInsecureAndHeaders(t *testing.T) {
	// Insecure (plaintext) + custom headers (a managed collector's auth token)
	// must not break construction — the gRPC exporter still dials lazily. Covers
	// the two conditional exporter options.
	p, err := Setup(context.Background(), enabled(func(c *Config) {
		c.Exporter = ExporterOTLP
		c.Endpoint = "localhost:4317"
		c.Sampler = SamplerAlwaysOn
		c.Insecure = true
		c.Headers = map[string]string{"x-api-key": "secret"}
	}))
	if err != nil {
		t.Fatalf("otlp setup with insecure+headers: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetupStdoutExporter(t *testing.T) {
	p, err := Setup(context.Background(), enabled(func(c *Config) {
		c.Exporter = ExporterStdout
		c.Sampler = SamplerAlwaysOn
	}))
	if err != nil {
		t.Fatalf("stdout setup: %v", err)
	}
	if p.shutdown == nil {
		t.Fatal("stdout provider should carry a shutdown func")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetupInvalidConfigRejected(t *testing.T) {
	if _, err := Setup(context.Background(), enabled(func(c *Config) { c.Sampler = "nope" })); err == nil {
		t.Fatal("expected setup to reject invalid sampler")
	}
}

func TestResolveSamplerAll(t *testing.T) {
	for _, s := range []Sampler{SamplerAlwaysOn, SamplerAlwaysOff, SamplerTraceRatio, SamplerParentBasedTraceRatio, "fallback"} {
		if resolveSampler(enabled(func(c *Config) { c.Sampler = s })) == nil {
			t.Fatalf("nil sampler for %q", s)
		}
	}
}

func TestBridgeRoundTrip(t *testing.T) {
	id := uuid.New()
	if got := UUIDFromTraceID(TraceIDFromUUID(id)); got != id {
		t.Fatalf("round trip: got %s want %s", got, id)
	}
}

func TestSlogHandlerEnrichesFromSpanContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, nil)))

	tid := TraceIDFromUUID(uuid.New())
	sid := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "with span")
	logger.Info("no context span")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["traceId"] != tid.String() {
		t.Fatalf("traceId = %v want %s", first["traceId"], tid.String())
	}
	if first["spanId"] != sid.String() {
		t.Fatalf("spanId = %v want %s", first["spanId"], sid.String())
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if _, ok := second["traceId"]; ok {
		t.Fatal("line without span context must not carry traceId")
	}
}

func TestSlogHandlerEnabledWithAttrsWithGroup(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewSlogHandler(base)
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be enabled")
	}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug below threshold should be disabled")
	}
	// WithAttrs / WithGroup return wrapped handlers that still enrich.
	logger := slog.New(h.WithAttrs([]slog.Attr{slog.String("svc", "x")}).WithGroup("g"))
	logger.Info("hi")
	if !strings.Contains(buf.String(), "\"svc\":\"x\"") {
		t.Fatalf("WithAttrs lost: %s", buf.String())
	}
}

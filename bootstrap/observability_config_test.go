package bootstrap

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

func TestTracingConfigDefaultsDisabledUntouched(t *testing.T) {
	var o ObservabilityConfig // zero value: tracing off
	o.applyDefaults()
	o.applyProfileDefaults(profileDev)
	if o.Tracing.Enabled {
		t.Fatal("absent block must stay disabled")
	}
	// No defaults applied to a disabled block.
	if o.Tracing.Exporter != "" || o.Tracing.Sampler != "" || o.Tracing.Ratio != 0 {
		t.Fatalf("disabled block should not be defaulted, got %+v", o.Tracing)
	}
	if err := o.validate(); err != nil {
		t.Fatalf("disabled validate: %v", err)
	}
}

func TestTracingConfigDefaultsEnabled(t *testing.T) {
	o := ObservabilityConfig{Tracing: TracingConfig{Enabled: true}}
	o.applyDefaults()
	if o.Tracing.Exporter != string(tracing.ExporterOTLP) {
		t.Errorf("exporter default = %q", o.Tracing.Exporter)
	}
	if o.Tracing.Endpoint != "localhost:4317" {
		t.Errorf("endpoint default = %q", o.Tracing.Endpoint)
	}
	if o.Tracing.Ratio != 0.1 {
		t.Errorf("ratio default = %v", o.Tracing.Ratio)
	}
}

func TestTracingProfileSamplerDefaults(t *testing.T) {
	dev := ObservabilityConfig{Tracing: TracingConfig{Enabled: true}}
	dev.applyDefaults()
	dev.applyProfileDefaults(profileDev)
	if dev.Tracing.Sampler != string(tracing.SamplerAlwaysOn) {
		t.Errorf("dev sampler = %q, want always_on", dev.Tracing.Sampler)
	}

	prd := ObservabilityConfig{Tracing: TracingConfig{Enabled: true}}
	prd.applyDefaults()
	prd.applyProfileDefaults("prd")
	if prd.Tracing.Sampler != string(tracing.SamplerParentBasedTraceRatio) {
		t.Errorf("prd sampler = %q, want parentbased_traceratio", prd.Tracing.Sampler)
	}

	// Explicit sampler wins over the profile default.
	exp := ObservabilityConfig{Tracing: TracingConfig{Enabled: true, Sampler: string(tracing.SamplerAlwaysOff)}}
	exp.applyProfileDefaults(profileDev)
	if exp.Tracing.Sampler != string(tracing.SamplerAlwaysOff) {
		t.Errorf("explicit sampler overridden: %q", exp.Tracing.Sampler)
	}
}

func TestTracingProfileInsecureDefaults(t *testing.T) {
	dev := ObservabilityConfig{Tracing: TracingConfig{Enabled: true}}
	dev.applyProfileDefaults(profileDev)
	if dev.Tracing.Insecure == nil || !*dev.Tracing.Insecure {
		t.Errorf("dev insecure = %v, want true (plaintext local collector)", dev.Tracing.Insecure)
	}

	prd := ObservabilityConfig{Tracing: TracingConfig{Enabled: true}}
	prd.applyProfileDefaults("prd")
	if prd.Tracing.Insecure == nil || *prd.Tracing.Insecure {
		t.Errorf("prd insecure = %v, want false (TLS)", prd.Tracing.Insecure)
	}

	// Explicit insecure wins over the profile default.
	no := false
	exp := ObservabilityConfig{Tracing: TracingConfig{Enabled: true, Insecure: &no}}
	exp.applyProfileDefaults(profileDev)
	if exp.Tracing.Insecure == nil || *exp.Tracing.Insecure {
		t.Errorf("explicit insecure=false overridden by dev default: %v", exp.Tracing.Insecure)
	}
}

func TestTracingValidate(t *testing.T) {
	ok := ObservabilityConfig{Tracing: TracingConfig{Enabled: true, Exporter: "otlp", Sampler: "always_on", Ratio: 0.5, Instrument: []string{"http", "relational"}}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Empty sampler tolerated at validate time (profile default applied later).
	emptySampler := ObservabilityConfig{Tracing: TracingConfig{Enabled: true, Exporter: "otlp"}}
	if err := emptySampler.validate(); err != nil {
		t.Fatalf("empty sampler should be tolerated: %v", err)
	}

	bad := map[string]TracingConfig{
		"exporter": {Enabled: true, Exporter: "weird"},
		"sampler":  {Enabled: true, Exporter: "otlp", Sampler: "weird"},
		"ratio":    {Enabled: true, Exporter: "otlp", Ratio: 2},
		"subsys":   {Enabled: true, Exporter: "otlp", Instrument: []string{"http", "nope"}},
	}
	for name, tc := range bad {
		o := ObservabilityConfig{Tracing: tc}
		if err := o.validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestTracingResolve(t *testing.T) {
	// Empty instrument list expands to all subsystems; service name defaults.
	o := TracingConfig{Enabled: true, Exporter: "otlp", Endpoint: "h:4317", Sampler: "always_on", Ratio: 0.2}
	cfg := o.Resolve("billing")
	if cfg.ServiceName != "billing" {
		t.Errorf("serviceName = %q", cfg.ServiceName)
	}
	for _, s := range tracing.AllSubsystems() {
		if !cfg.Instrument[s] {
			t.Errorf("empty list should enable %s", s)
		}
	}
	// Explicit list = allowlist; explicit serviceName wins.
	o2 := TracingConfig{Enabled: true, ServiceName: "custom", Instrument: []string{"relational"}}
	cfg2 := o2.Resolve("billing")
	if cfg2.ServiceName != "custom" {
		t.Errorf("explicit serviceName lost: %q", cfg2.ServiceName)
	}
	if !cfg2.Instrument[tracing.SubRelational] || cfg2.Instrument[tracing.SubMongo] {
		t.Errorf("allowlist not honored: %+v", cfg2.Instrument)
	}

	// Insecure + headers thread through to the resolved Config.
	yes := true
	o3 := TracingConfig{Enabled: true, Insecure: &yes, Headers: map[string]string{"x-api-key": "k"}}
	cfg3 := o3.Resolve("svc")
	if !cfg3.Insecure {
		t.Error("insecure not threaded into Config")
	}
	if cfg3.Headers["x-api-key"] != "k" {
		t.Errorf("headers not threaded: %+v", cfg3.Headers)
	}
	// Nil insecure resolves to false (the profile default is applied earlier, in
	// applyProfileDefaults, not here).
	if (TracingConfig{Enabled: true}).Resolve("svc").Insecure {
		t.Error("nil insecure should resolve to false")
	}
}

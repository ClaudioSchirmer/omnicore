package integration

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

type fakeRequest struct {
	Email string `json:"email"`
}

type fakeCommand struct {
	Email string
}

func (r fakeRequest) ToCommand() *fakeCommand {
	return &fakeCommand{Email: r.Email}
}

type fakeHandler struct {
	called bool
}

type fakeResult struct {
	OK bool
}

func (r fakeResult) IsSuccess() bool { return r.OK }

func (h *fakeHandler) Handle(_ *configuration.AppContext, cmd *fakeCommand) (fakeResult, error) {
	h.called = true
	return fakeResult{OK: true}, nil
}

func TestRegistryFromOn(t *testing.T) {
	reg := NewRegistry()
	h := &fakeHandler{}
	reg.From("partners").On("partnerOnboarded", fakeRequest{}, h)

	receivers := reg.Receivers()
	if len(receivers) != 1 {
		t.Fatalf("expected 1 receiver, got %d", len(receivers))
	}
	if reg.IsEmpty() {
		t.Fatal("registry should not be empty")
	}
	r := receivers[0]
	if r.SourceKey() != "partners" {
		t.Errorf("expected sourceKey partners, got %q", r.SourceKey())
	}
	if r.EventKey() != "partnerOnboarded" {
		t.Errorf("expected eventKey partnerOnboarded, got %q", r.EventKey())
	}
}

func TestRegistryFromOnPanicsOnNilSample(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil sample")
		}
	}()
	NewRegistry().From("x").On("y", nil, &fakeHandler{})
}

func TestRegistryFromOnPanicsOnNilHandler(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil handler")
		}
	}()
	NewRegistry().From("x").On("y", fakeRequest{}, nil)
}

func TestReceiverResolveAgainstYAML(t *testing.T) {
	reg := NewRegistry()
	reg.From("partners").On("partnerOnboarded", fakeRequest{}, &fakeHandler{})

	cfg := &Config{
		Subscribes: map[string]SubscribeEntry{
			"partners": {
				Topic:         "partners.integration.events",
				ConsumerGroup: "orders-integration",
				Workers:       2,
				StartFrom:     "latest",
				Events: map[string]SubscribeEvent{
					"partnerOnboarded": {EventType: "PartnerOnboarded"},
				},
			},
		},
	}

	r := reg.Receivers()[0]
	if err := r.resolveAgainstYAML(cfg); err != nil {
		t.Fatalf("resolveAgainstYAML: %v", err)
	}
	if r.Topic() != "partners.integration.events" {
		t.Errorf("topic: %q", r.Topic())
	}
	if r.WireEventType() != "PartnerOnboarded" {
		t.Errorf("wireEventType: %q", r.WireEventType())
	}
	if r.ConsumerGroup() != "orders-integration" {
		t.Errorf("consumerGroup: %q", r.ConsumerGroup())
	}
}

func TestReceiverResolveAgainstYAMLMissingSource(t *testing.T) {
	reg := NewRegistry()
	reg.From("missing").On("x", fakeRequest{}, &fakeHandler{})
	cfg := &Config{Subscribes: map[string]SubscribeEntry{}}
	err := reg.Receivers()[0].resolveAgainstYAML(cfg)
	if err == nil {
		t.Fatal("expected error for missing source key")
	}
}

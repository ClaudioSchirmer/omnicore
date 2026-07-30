package bootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUpstreamSubscription_YAMLRoundTrip(t *testing.T) {
	src := `
topic: users.events
collection: users
consumerGroup: orders-upstream-users
workers: 2
fields:
  - name
  - email
deleteOnArchive: false
startFrom: latest
onUpstreamDelete: anonymize
anonymizeFields:
  - name
  - email
acknowledgeOffsetReset: false
`
	var got UpstreamSubscription
	if err := yaml.Unmarshal([]byte(src), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Topic != "users.events" || got.Collection != "users" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.ConsumerGroup != "orders-upstream-users" {
		t.Errorf("ConsumerGroup = %q", got.ConsumerGroup)
	}
	if got.Workers != 2 {
		t.Errorf("Workers = %d", got.Workers)
	}
	if len(got.Fields) != 2 || got.Fields[0] != "name" || got.Fields[1] != "email" {
		t.Errorf("Fields = %v", got.Fields)
	}
	if got.OnUpstreamDelete != UpstreamDeleteAnonymize {
		t.Errorf("OnUpstreamDelete = %q", got.OnUpstreamDelete)
	}
	if got.StartFrom != StartFromLatest {
		t.Errorf("StartFrom = %q", got.StartFrom)
	}
	if len(got.AnonymizeFields) != 2 {
		t.Errorf("AnonymizeFields = %v", got.AnonymizeFields)
	}
	if got.DeleteOnArchive || got.AcknowledgeOffsetReset {
		t.Errorf("boolean defaults flipped: %+v", got)
	}
}

func TestUpstreamSubscription_UnmarshalYAML_RejectsUnknownField(t *testing.T) {
	src := `
topic: users.events
collection: users
anonymizeFiels: [name] # intentional typo
`
	var got UpstreamSubscription
	err := yaml.Unmarshal([]byte(src), &got)
	if err == nil {
		t.Fatal("expected error on unknown field")
	}
	if !strings.Contains(err.Error(), "anonymizeFiels") {
		t.Errorf("error should name the bad key: %v", err)
	}
}

func TestUpstreamSubscription_ApplyDefaults(t *testing.T) {
	s := UpstreamSubscription{Topic: "users.events", Collection: "users"}
	s.applyDefaults("orders")
	if s.ConsumerGroup != "orders-upstream-users.events" {
		t.Errorf("ConsumerGroup default = %q", s.ConsumerGroup)
	}
	if s.Workers != 1 {
		t.Errorf("Workers default = %d", s.Workers)
	}
	if s.StartFrom != StartFromLatest {
		t.Errorf("StartFrom default = %q", s.StartFrom)
	}
	if s.OnUpstreamDelete != UpstreamDeleteCascade {
		t.Errorf("OnUpstreamDelete default = %q", s.OnUpstreamDelete)
	}
}

func TestUpstreamSubscription_ApplyDefaults_KeepsExplicit(t *testing.T) {
	s := UpstreamSubscription{
		Topic:            "users.events",
		Collection:       "users",
		ConsumerGroup:    "explicit-group",
		Workers:          4,
		StartFrom:        StartFromEarliest,
		OnUpstreamDelete: UpstreamDeleteKeep,
	}
	s.applyDefaults("orders")
	if s.ConsumerGroup != "explicit-group" || s.Workers != 4 ||
		s.StartFrom != StartFromEarliest || s.OnUpstreamDelete != UpstreamDeleteKeep {
		t.Errorf("explicit values overwritten by defaults: %+v", s)
	}
}

func TestUpstreamSubscription_ValidateShape_ValidValues(t *testing.T) {
	cases := []struct {
		name string
		s    UpstreamSubscription
	}{
		{"latest+cascade", UpstreamSubscription{
			Topic: "t", Collection: "c",
			StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteCascade,
		}},
		{"earliest+keep", UpstreamSubscription{
			Topic: "t", Collection: "c",
			StartFrom: StartFromEarliest, OnUpstreamDelete: UpstreamDeleteKeep,
		}},
		{"anonymize", UpstreamSubscription{
			Topic: "t", Collection: "c",
			StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteAnonymize,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.s.validateShape(profileDev); err != nil {
				t.Errorf("expected ok, got: %v", err)
			}
		})
	}
}

func TestUpstreamSubscription_ValidateShape_RejectsInvalidStartFrom(t *testing.T) {
	s := UpstreamSubscription{Topic: "t", Collection: "c",
		StartFrom: "tomorrow", OnUpstreamDelete: UpstreamDeleteCascade}
	if err := s.validateShape(profileDev); err == nil || !strings.Contains(err.Error(), "startFrom") {
		t.Errorf("expected startFrom error, got %v", err)
	}
}

func TestUpstreamSubscription_ValidateShape_RejectsInvalidOnUpstreamDelete(t *testing.T) {
	s := UpstreamSubscription{Topic: "t", Collection: "c",
		StartFrom: StartFromLatest, OnUpstreamDelete: "nuke"}
	if err := s.validateShape(profileDev); err == nil || !strings.Contains(err.Error(), "onUpstreamDelete") {
		t.Errorf("expected onUpstreamDelete error, got %v", err)
	}
}

func TestUpstreamSubscription_ValidateShape_AcceptsOffsetUnderDev(t *testing.T) {
	s := UpstreamSubscription{Topic: "t", Collection: "c",
		StartFrom: "offset:42", OnUpstreamDelete: UpstreamDeleteCascade}
	if err := s.validateShape(profileDev); err != nil {
		t.Errorf("offset:N should be accepted under dev, got %v", err)
	}
}

func TestUpstreamSubscription_ValidateShape_OffsetUnderProdRequiresAck(t *testing.T) {
	s := UpstreamSubscription{Topic: "t", Collection: "c",
		StartFrom: "offset:42", OnUpstreamDelete: UpstreamDeleteCascade}
	err := s.validateShape("prd")
	if err == nil || !strings.Contains(err.Error(), "acknowledgeOffsetReset") {
		t.Errorf("expected acknowledgeOffsetReset error, got %v", err)
	}
	s.AcknowledgeOffsetReset = true
	if err := s.validateShape("prd"); err != nil {
		t.Errorf("after ack, offset:N should be accepted under prd, got %v", err)
	}
}

func TestUpstreamSubscription_ValidateShape_RejectsMalformedOffset(t *testing.T) {
	s := UpstreamSubscription{Topic: "t", Collection: "c",
		StartFrom: "offset:-1", OnUpstreamDelete: UpstreamDeleteCascade}
	// Note: regex requires [0-9]+ so "-1" doesn't match; the error
	// surfaces via the startFrom branch, not the negative check.
	if err := s.validateShape(profileDev); err == nil ||
		!strings.Contains(err.Error(), "startFrom") {
		t.Errorf("expected startFrom rejection for %q, got %v", s.StartFrom, err)
	}
}

func TestUpstreamSubscription_ValidateShape_RequiresTopicAndCollection(t *testing.T) {
	if err := (UpstreamSubscription{Collection: "c",
		StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteCascade}).validateShape(profileDev); err == nil ||
		!strings.Contains(err.Error(), "topic") {
		t.Errorf("expected topic missing error, got %v", err)
	}
	if err := (UpstreamSubscription{Topic: "t",
		StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteCascade}).validateShape(profileDev); err == nil ||
		!strings.Contains(err.Error(), "collection") {
		t.Errorf("expected collection missing error, got %v", err)
	}
}

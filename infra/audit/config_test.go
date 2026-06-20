package audit

import (
	"strings"
	"testing"
)

func TestConfig_ApplyDefaults_NilGetsBothDestinations(t *testing.T) {
	c := &Config{} // Destinations nil → "use the framework default"
	c.ApplyDefaults()
	if len(c.Destinations) != 2 {
		t.Fatalf("default destinations = %v, want [slog database]", c.Destinations)
	}
	if !c.Includes(DestinationSlog) || !c.Includes(DestinationDatabase) {
		t.Errorf("default must include both slog and database, got %v", c.Destinations)
	}
}

func TestConfig_ApplyDefaults_EmptySlicePreservedAsOff(t *testing.T) {
	// A declared but empty slice is an explicit "audit off" — preserved verbatim.
	c := &Config{Destinations: []Destination{}}
	c.ApplyDefaults()
	if c.Destinations == nil || len(c.Destinations) != 0 {
		t.Errorf("explicit empty slice must stay empty (audit off), got %v", c.Destinations)
	}
}

func TestConfig_ApplyDefaults_PopulatedUntouched(t *testing.T) {
	c := &Config{Destinations: []Destination{DestinationSlog}}
	c.ApplyDefaults()
	if len(c.Destinations) != 1 || c.Destinations[0] != DestinationSlog {
		t.Errorf("populated slice must be untouched, got %v", c.Destinations)
	}
}

func TestConfig_Validate_AcceptsKnownValues(t *testing.T) {
	cases := [][]Destination{
		nil,
		{},
		{DestinationSlog},
		{DestinationDatabase},
		{DestinationSlog, DestinationDatabase},
	}
	for i, dests := range cases {
		c := &Config{Destinations: dests}
		if err := c.Validate(); err != nil {
			t.Errorf("case %d (%v): unexpected error %v", i, dests, err)
		}
	}
}

func TestConfig_Validate_RejectsUnknownValue(t *testing.T) {
	c := &Config{Destinations: []Destination{DestinationSlog, "kafka"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown value") || !strings.Contains(err.Error(), "kafka") {
		t.Errorf("expected unknown-value error naming the offender, got %v", err)
	}
}

func TestConfig_Validate_RejectsDuplicate(t *testing.T) {
	c := &Config{Destinations: []Destination{DestinationSlog, DestinationSlog}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestConfig_Includes(t *testing.T) {
	c := &Config{Destinations: []Destination{DestinationDatabase}}
	if !c.Includes(DestinationDatabase) {
		t.Error("Includes(database) must be true when declared")
	}
	if c.Includes(DestinationSlog) {
		t.Error("Includes(slog) must be false when not declared")
	}
	// Empty config includes nothing (audit off).
	if (&Config{}).Includes(DestinationDatabase) {
		t.Error("empty config must include nothing")
	}
}

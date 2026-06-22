package queries

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

func assertForbiddenCarrier(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a 403 error, got nil")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier, got %T", err)
	}
	if len(carrier.NotificationContexts()) == 0 {
		t.Fatal("carrier carries no notification contexts")
	}
}

func TestReadCriteria_Restrict_PassiveWholeDocExcludesSilently(t *testing.T) {
	c := ReadCriteria{}
	if err := c.Restrict("Salary"); err != nil {
		t.Fatalf("passive Restrict should not error, got %v", err)
	}
	if v, ok := c.Projection["Salary"]; !ok || v != 0 {
		t.Errorf("expected Salary excluded ({Salary:0}), got %v ok=%v", v, ok)
	}
}

func TestReadCriteria_Restrict_ActiveSortIs403AndScrubs(t *testing.T) {
	c := ReadCriteria{Sort: []SortField{{Field: "Salary"}, {Field: "Name"}}}
	assertForbiddenCarrier(t, c.Restrict("Salary"))
	if len(c.Sort) != 1 || c.Sort[0].Field != "Name" {
		t.Errorf("Salary sort scrubbed + other sorts survive — got %v", c.Sort)
	}
}

func TestReadCriteria_Restrict_ActiveFilterIs403AndScrubs(t *testing.T) {
	c := ReadCriteria{Filter: map[string]any{"Salary": 100, "Name": "x"}}
	assertForbiddenCarrier(t, c.Restrict("Salary"))
	if _, ok := c.Filter["Salary"]; ok {
		t.Error("Salary filter must be scrubbed")
	}
	if _, ok := c.Filter["Name"]; !ok {
		t.Error("other filters must survive")
	}
}

func TestReadCriteria_Restrict_ActiveFieldsIs403AndDropsInclude(t *testing.T) {
	c := ReadCriteria{Projection: map[string]int{"Salary": 1, "Name": 1}}
	assertForbiddenCarrier(t, c.Restrict("Salary"))
	if _, ok := c.Projection["Salary"]; ok {
		t.Error("Salary include must be dropped in inclusion mode")
	}
	if c.Projection["Name"] != 1 {
		t.Error("other includes must survive")
	}
}

func TestReadCriteria_Restrict_PassiveInclusionModeNoExclusionLeak(t *testing.T) {
	// ?fields=name (Salary not requested): Salary is passively absent — no 403,
	// and no {Salary:0} is mixed into an inclusion projection (Mongo forbids it).
	c := ReadCriteria{Projection: map[string]int{"Name": 1}}
	if err := c.Restrict("Salary"); err != nil {
		t.Fatalf("passive Restrict in inclusion mode should not error, got %v", err)
	}
	if _, ok := c.Projection["Salary"]; ok {
		t.Error("Salary must not appear in an inclusion projection")
	}
	if c.Projection["Name"] != 1 {
		t.Error("Name include must survive")
	}
}

func TestFieldAccessForbiddenNotification_SemanticIsForbidden(t *testing.T) {
	if got := (notifications.FieldAccessForbiddenNotification{}).Semantic(); got != domain.SemanticForbidden {
		t.Errorf("Semantic = %v, want SemanticForbidden (403)", got)
	}
}

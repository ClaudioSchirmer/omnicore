package domain

import "testing"

func TestNotificationBaseDefaultSemantic(t *testing.T) {
	type CustomNotification struct{ NotificationBase }
	var n CustomNotification
	if got := n.Semantic(); got != SemanticValidation {
		t.Fatalf("expected SemanticValidation default, got %v", got)
	}
}

func TestDomainNotificationBaseInheritsDefault(t *testing.T) {
	type CustomNotification struct{ DomainNotificationBase }
	var n CustomNotification
	if got := n.Semantic(); got != SemanticValidation {
		t.Fatalf("expected SemanticValidation default, got %v", got)
	}
}

func TestRecordNotFoundSemantic(t *testing.T) {
	if got := (RecordNotFoundNotification{}).Semantic(); got != SemanticNotFound {
		t.Fatalf("expected SemanticNotFound, got %v", got)
	}
}

func TestEntityIsNotActiveSemantic(t *testing.T) {
	if got := (EntityIsNotActiveNotification{}).Semantic(); got != SemanticConflict {
		t.Fatalf("expected SemanticConflict, got %v", got)
	}
}

func TestEntityAlreadyAddedSemantic(t *testing.T) {
	if got := (EntityAlreadyAddedNotification{}).Semantic(); got != SemanticConflict {
		t.Fatalf("expected SemanticConflict, got %v", got)
	}
}

func TestNotAllowedSemantics(t *testing.T) {
	cases := []struct {
		name string
		got  NotificationSemantic
	}{
		{"InsertNotAllowedNotification", (InsertNotAllowedNotification{}).Semantic()},
		{"UpdateNotAllowedNotification", (UpdateNotAllowedNotification{}).Semantic()},
		{"DeleteNotAllowedNotification", (DeleteNotAllowedNotification{}).Semantic()},
	}
	for _, c := range cases {
		if c.got != SemanticForbidden {
			t.Errorf("%s: expected SemanticForbidden, got %v", c.name, c.got)
		}
	}
}

// emailAlreadyExistsNotification stands in for a typical service-side
// notification that overrides Semantic() to mark a uniqueness violation as
// 409 Conflict. Method overrides on types defined inside a func body do not
// satisfy interfaces, so this type lives at package scope.
type emailAlreadyExistsNotification struct{ DomainNotificationBase }

func (emailAlreadyExistsNotification) Semantic() NotificationSemantic { return SemanticConflict }

func TestServiceSideOverride(t *testing.T) {
	var n Notification = emailAlreadyExistsNotification{}
	if got := n.Semantic(); got != SemanticConflict {
		t.Fatalf("expected SemanticConflict, got %v", got)
	}
}

func TestNotificationSemanticString(t *testing.T) {
	cases := map[NotificationSemantic]string{
		SemanticValidation:   "Validation",
		SemanticNotFound:     "NotFound",
		SemanticConflict:     "Conflict",
		SemanticForbidden:    "Forbidden",
		SemanticUnauthorized: "Unauthorized",
		SemanticUnavailable:  "Unavailable",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%v.String(): want %q, got %q", s, want, got)
		}
	}
}

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
	if got := (EntityIsNotActiveNotification{}).Semantic(); got != SemanticStateConflict {
		t.Fatalf("expected SemanticStateConflict, got %v", got)
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
		SemanticValidation:     "Validation",
		SemanticNotFound:       "NotFound",
		SemanticConflict:       "Conflict",
		SemanticForbidden:      "Forbidden",
		SemanticUnauthorized:   "Unauthorized",
		SemanticUnavailable:    "Unavailable",
		SemanticGatewayTimeout: "GatewayTimeout",
		SemanticStateConflict:  "StateConflict",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%v.String(): want %q, got %q", s, want, got)
		}
	}
}

// TestNotificationSemanticString_BasicHTTPFamily locks the wire label of the
// six statuses added to close the everyday-HTTP gap. The label is what lands
// in the REST envelope's `semantic`, the gRPC ErrorInfo metadata and the
// GraphQL extension — a regression here is a silent contract change on three
// surfaces at once.
func TestNotificationSemanticString_BasicHTTPFamily(t *testing.T) {
	cases := map[NotificationSemantic]string{
		SemanticGone:                 "Gone",
		SemanticPreconditionFailed:   "PreconditionFailed",
		SemanticUnsupportedMediaType: "UnsupportedMediaType",
		SemanticTooManyRequests:      "TooManyRequests",
		SemanticNotImplemented:       "NotImplemented",
		SemanticBadGateway:           "BadGateway",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(s), got, want)
		}
	}
}

// TestNotificationSemantic_NoDuplicateLabels asserts the enum is injective on
// String(): two semantics sharing a label would make the wire ambiguous
// exactly where the split exists to disambiguate (Conflict vs StateConflict,
// MethodNotAllowed vs NotImplemented, Unavailable vs BadGateway).
func TestNotificationSemantic_NoDuplicateLabels(t *testing.T) {
	seen := map[string]NotificationSemantic{}
	for s := SemanticValidation; s <= SemanticBadGateway; s++ {
		label := s.String()
		if prev, dup := seen[label]; dup {
			t.Errorf("semantics %d and %d share the label %q", int(prev), int(s), label)
		}
		seen[label] = s
	}
}

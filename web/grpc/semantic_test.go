package grpc

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

func dtosWith(sem domain.NotificationSemantic, key, field, msg string) []notifications.ContextDTO {
	return []notifications.ContextDTO{{
		Context: "Test",
		Messages: []notifications.MessageDTO{{
			NotificationKey: key,
			FieldName:       field,
			Message:         msg,
			Semantic:        sem,
		}},
	}}
}

func TestCodeFromNotificationsTable(t *testing.T) {
	cases := map[domain.NotificationSemantic]connect.Code{
		domain.SemanticSchema:               connect.CodeInvalidArgument,
		domain.SemanticNotFound:             connect.CodeNotFound,
		domain.SemanticConflict:             connect.CodeAlreadyExists,
		domain.SemanticStateConflict:        connect.CodeFailedPrecondition,
		domain.SemanticForbidden:            connect.CodePermissionDenied,
		domain.SemanticUnauthorized:         connect.CodeUnauthenticated,
		domain.SemanticUnavailable:          connect.CodeUnavailable,
		domain.SemanticInternal:             connect.CodeInternal,
		domain.SemanticMethodNotAllowed:     connect.CodeUnimplemented,
		domain.SemanticPayloadTooLarge:      connect.CodeResourceExhausted,
		domain.SemanticGatewayTimeout:       connect.CodeDeadlineExceeded,
		domain.SemanticRequestTimeout:       connect.CodeDeadlineExceeded,
		domain.SemanticGone:                 connect.CodeNotFound,
		domain.SemanticPreconditionFailed:   connect.CodeFailedPrecondition,
		domain.SemanticUnsupportedMediaType: connect.CodeInvalidArgument,
		domain.SemanticTooManyRequests:      connect.CodeResourceExhausted,
		domain.SemanticNotImplemented:       connect.CodeUnimplemented,
		domain.SemanticBadGateway:           connect.CodeUnavailable,
	}
	for sem, want := range cases {
		if got := codeFromNotifications(dtosWith(sem, "K", "", "m")); got != want {
			t.Errorf("semantic %v: want %v, got %v", sem, want, got)
		}
	}
}

func TestCodeFromNotificationsAllValidationFallsBack(t *testing.T) {
	if got := codeFromNotifications(dtosWith(domain.SemanticValidation, "K", "f", "m")); got != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", got)
	}
	if got := codeFromNotifications(nil); got != connect.CodeInvalidArgument {
		t.Fatalf("empty: want InvalidArgument, got %v", got)
	}
}

func TestCodeFromNotificationsMixedFavorsNonValidation(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Test",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "A", Semantic: domain.SemanticValidation, Message: "v"},
			{NotificationKey: "B", Semantic: domain.SemanticStateConflict, Message: "c"},
		},
	}}
	if got := codeFromNotifications(dtos); got != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", got)
	}
}

func TestCodeFromNotificationsUnknownSemanticFallsBack(t *testing.T) {
	if got := codeFromNotifications(dtosWith(domain.NotificationSemantic(999), "K", "", "m")); got != connect.CodeInvalidArgument {
		t.Fatalf("unknown semantic: want InvalidArgument, got %v", got)
	}
}

func decodeDetails(t *testing.T, cerr *connect.Error) (reasons []string, metadata []map[string]string, violations map[string]string) {
	t.Helper()
	violations = map[string]string{}
	for _, d := range cerr.Details() {
		v, err := d.Value()
		if err != nil {
			t.Fatalf("detail decode: %v", err)
		}
		switch m := v.(type) {
		case *errdetails.ErrorInfo:
			reasons = append(reasons, m.GetReason())
			metadata = append(metadata, m.GetMetadata())
		case *errdetails.BadRequest:
			for _, fv := range m.GetFieldViolations() {
				violations[fv.GetField()] = fv.GetDescription()
			}
		}
	}
	return reasons, metadata, violations
}

func TestErrorFromNotificationsDetails(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Gadget",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "GadgetNameRequiredNotification", FieldName: "name", Message: "Name required.", Semantic: domain.SemanticValidation},
			{NotificationKey: "EntityIsNotActiveNotification", Message: "Not active.", Semantic: domain.SemanticStateConflict},
		},
	}}
	cerr := ErrorFromNotifications(dtos)
	if cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", cerr.Code())
	}
	if cerr.Message() != "Name required." {
		t.Fatalf("first message must lead: %q", cerr.Message())
	}
	reasons, metadata, violations := decodeDetails(t, cerr)
	if len(reasons) != 2 || reasons[0] != "GadgetNameRequiredNotification" || reasons[1] != "EntityIsNotActiveNotification" {
		t.Fatalf("reasons: %v", reasons)
	}
	if metadata[0]["context"] != "Gadget" || metadata[0]["field"] != "name" || metadata[0]["semantic"] != "Validation" {
		t.Fatalf("metadata[0]: %v", metadata[0])
	}
	if metadata[1]["semantic"] != "StateConflict" {
		t.Fatalf("metadata[1]: %v", metadata[1])
	}
	if _, hasField := metadata[1]["field"]; hasField {
		t.Fatalf("field metadata must be omitted when empty: %v", metadata[1])
	}
	if violations["name"] != "Name required." {
		t.Fatalf("violations: %v", violations)
	}
	if len(violations) != 1 {
		t.Fatalf("only field-scoped messages violate: %v", violations)
	}
}

func TestErrorFromNotificationsEmptyEnvelope(t *testing.T) {
	cerr := ErrorFromNotifications(nil)
	if cerr.Code() != connect.CodeInvalidArgument || cerr.Message() != "request rejected" {
		t.Fatalf("empty envelope: %v / %q", cerr.Code(), cerr.Message())
	}
	if len(cerr.Details()) != 0 {
		t.Fatalf("no details expected")
	}
}

func TestErrInternalIsOpaque(t *testing.T) {
	cerr := errInternal()
	if cerr.Code() != connect.CodeInternal || cerr.Message() != "internal server error" {
		t.Fatalf("got %v / %q", cerr.Code(), cerr.Message())
	}
}

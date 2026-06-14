package notifications

import "github.com/ClaudioSchirmer/omnicore/domain"

type ContextDTO struct {
	Context  string       `json:"context"`
	Messages []MessageDTO `json:"messages"`
}

type MessageDTO struct {
	// NotificationKey is the unqualified Go type name of the source Notification
	// (e.g. "RequiredFieldNotification"). Preserved through translation so the
	// HTTP layer can map known notifications to specific status codes, and so
	// clients can branch UI on a stable typed identity.
	NotificationKey string `json:"notificationKey,omitempty"`
	FieldName       string `json:"field,omitempty"`
	FieldValue      string `json:"value,omitempty"`
	FuncName        string `json:"funcName,omitempty"`
	Message         string `json:"message"`
	// Semantic is the transport-agnostic classification of the source Notification.
	// The web layer maps it to an HTTP status code (e.g. SemanticNotFound → 404).
	Semantic domain.NotificationSemantic `json:"-"`
}

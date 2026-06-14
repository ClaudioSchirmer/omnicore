package domain

import (
	"encoding/json"

	"github.com/google/uuid"
)

type ID struct {
	value string
}

// MarshalJSON serializes ID as a plain JSON string carrying its canonical
// value, so that Response.Data populated with an ID renders as `"<uuid>"`
// rather than the zero-object `{}` Go would produce by default for a struct
// with no exported fields.
func (id ID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.value)
}

func NewID(s string) ID {
	return ID{value: s}
}

func NewIDFromUUID(u uuid.UUID) ID {
	return ID{value: u.String()}
}

func NewRandomID() ID {
	return ID{value: uuid.NewString()}
}

func (id ID) Value() string {
	return id.value
}

func (id ID) IsEmpty() bool {
	return id.value == ""
}

func (id ID) UUID() (uuid.UUID, error) {
	return uuid.Parse(id.value)
}

func (id ID) IsValid(fieldName string, ctx *NotificationContext) bool {
	if _, err := uuid.Parse(id.value); err != nil {
		if ctx != nil {
			ctx.AddNotificationMessage(NotificationMessage{
				FieldName:    fieldName,
				FieldValue:   id.value,
				Err:          err,
				Notification: InvalidIDUUIDNotification{},
			})
		}
		return false
	}
	return true
}

func (id ID) String() string {
	return id.value
}

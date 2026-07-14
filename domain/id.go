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

// UnmarshalJSON restores ID from its JSON string form — the symmetric half of
// MarshalJSON, so an ID round-trips through every JSON boundary the framework
// crosses (entity snapshots in the outbox payload, audit maps, DTO mapping).
// Like NewID, it performs no uuid validation: the ID is an opaque identity
// wrapper and IsValid is the explicit validation seam.
func (id *ID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	id.value = s
	return nil
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

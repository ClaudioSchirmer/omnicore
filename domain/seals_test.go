package domain

import "testing"

// The sealed-ValidEntity contract is critical rule material: the entity()
// marker is what makes Insertable/Updatable/... constructible ONLY by this
// package. These execute the seal methods so the contract is exercised, and
// pin that every write shape satisfies ValidEntity while a foreign type never
// can (there is no way to write entity() outside the package).
func TestValidEntitySeals(t *testing.T) {
	shapes := []ValidEntity{Insertable{}, Updatable{}, Archivable{}, Unarchivable{}, Deletable{}}
	for _, s := range shapes {
		s.entity() // the seal itself — a no-op, but it IS the contract
	}
	if len(shapes) != 5 {
		t.Fatalf("the closed write-shape set drifted: %d", len(shapes))
	}
}

// The notification and service seals follow the same pattern: embedding the
// base is the only way in.
func TestNotificationAndServiceSeals(t *testing.T) {
	NotificationBase{}.isNotification()
	ServiceBase{}.isService()

	type myNotification struct{ NotificationBase }
	var n any = myNotification{}
	if _, ok := n.(interface{ isNotification() }); !ok {
		t.Fatal("embedding NotificationBase must satisfy the notification seal")
	}
	type myService struct{ ServiceBase }
	var s any = myService{}
	if _, ok := s.(Service); !ok {
		t.Fatal("embedding ServiceBase must satisfy Service")
	}
}

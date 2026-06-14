package migration

import "github.com/ClaudioSchirmer/omnicore/domain"

// MigrationDownMissingNotification — a .up.sql migration in the service
// directory has no matching .down.sql. Detected by ValidateDownExists at boot.
type MigrationDownMissingNotification struct{ domain.InfrastructureNotificationBase }

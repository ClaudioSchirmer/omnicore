package migration

import "github.com/ClaudioSchirmer/omnicore/domain"

// MigrationDownMissingNotification — a .up.sql migration in the service
// directory has no matching .down.sql. Detected by ValidateDownExists at boot.
type MigrationDownMissingNotification struct {
	domain.InfrastructureNotificationBase
}

// MigrationFilenameInvalidNotification — a .up.sql / .down.sql file in the
// service directory does not match the required "{version}_{name}" prefix
// (version is a non-negative integer). golang-migrate silently ignores such
// files, so the operator's SQL would never run while boot reports success.
// Detected by ValidateDownExists at boot.
type MigrationFilenameInvalidNotification struct {
	domain.InfrastructureNotificationBase
}

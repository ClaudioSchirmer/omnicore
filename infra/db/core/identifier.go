package core

// SafeIdentifier reports whether name matches the framework's SQL identifier
// allowlist (non-empty; ASCII letters, digits, underscore). Identifiers in the
// framework come from TableSchema declarations / framework constants, never user
// input — engines call it to validate before composing SQL (the MySQL engine's
// backtick quoter, the admin CLI). The panicking counterpart lives next to each
// engine's identifier quoter.
func SafeIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

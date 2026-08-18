package translation

import "testing"

// TestCatalogs_AuthorizationKeysPresent asserts that every built-in catalog
// carries the authorization-layer notification keys. Catalog drift (a key
// added to one language but not the others) would surface at runtime as
// the wire response carrying the raw notification name instead of a
// translated message — caught here before the test of the dependent layer.
func TestCatalogs_AuthorizationKeysPresent(t *testing.T) {
	authzKeys := []string{
		"MissingPermissionNotification",
		"TenantMissingNotification",
		"TenantMismatchNotification",
	}
	for lang, mod := range allBuiltinCatalogs() {
		entries := mod.Translations()
		for _, key := range authzKeys {
			v, ok := entries[key]
			if !ok {
				t.Errorf("catalog %s missing key %q", lang, key)
				continue
			}
			if v == "" {
				t.Errorf("catalog %s has empty translation for %q", lang, key)
			}
		}
	}
}

// TestCatalogs_KeySetsConsistent asserts every key present in the English
// catalog also appears in every other built-in. Pinning ENG as the
// reference is arbitrary but matches the framework's English-first
// language rule.
func TestCatalogs_KeySetsConsistent(t *testing.T) {
	ref := CoreENG().Translations()
	for lang, mod := range allBuiltinCatalogs() {
		if lang == "ENG" {
			continue
		}
		entries := mod.Translations()
		for key := range ref {
			if _, ok := entries[key]; !ok {
				t.Errorf("catalog %s missing key %q present in ENG", lang, key)
			}
		}
		for key := range entries {
			if _, ok := ref[key]; !ok {
				t.Errorf("catalog %s has key %q absent from ENG", lang, key)
			}
		}
	}
}

// TestCatalogs_ContextLabelsPresent asserts every built-in catalog carries a
// label for each NotificationContext name the FRAMEWORK itself constructs.
// These are keys the framework emits, so the framework — not the consuming
// service — owes the translation: a missing one surfaces as a
// translation.key.missing warn on the first 404/405/413/401/500 and as the
// raw English name in the wire envelope's `context` field.
func TestCatalogs_ContextLabelsPresent(t *testing.T) {
	contextKeys := []string{
		"Authorization",
		"Pipeline",
		"Request",
		"Route",
		"Schema",
		"Server",
	}
	for lang, mod := range allBuiltinCatalogs() {
		entries := mod.Translations()
		for _, key := range contextKeys {
			v, ok := entries[key]
			if !ok {
				t.Errorf("catalog %s missing context label %q", lang, key)
				continue
			}
			if v == "" {
				t.Errorf("catalog %s has empty context label for %q", lang, key)
			}
		}
	}
}

func allBuiltinCatalogs() map[string]Module {
	return map[string]Module{
		"PTBR": CorePTBR(),
		"ENG":  CoreENG(),
		"ES":   CoreES(),
		"FR":   CoreFR(),
		"DE":   CoreDE(),
		"IT":   CoreIT(),
		"NL":   CoreNL(),
	}
}

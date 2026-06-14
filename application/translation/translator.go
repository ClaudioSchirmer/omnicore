package translation

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

type Translator struct {
	mu           sync.RWMutex
	translations map[configuration.Language]map[string]string
}

func New() *Translator {
	return &Translator{
		translations: map[configuration.Language]map[string]string{},
	}
}

func (t *Translator) Import(modules ...Module) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range modules {
		lang := m.Language()
		if _, ok := t.translations[lang]; !ok {
			t.translations[lang] = map[string]string{}
		}
		for k, v := range m.Translations() {
			t.translations[lang][k] = v
		}
	}
}

func (t *Translator) Get(lang configuration.Language, key string) (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if m, ok := t.translations[lang]; ok {
		if v, ok := m[key]; ok {
			return v, nil
		}
	}
	return "", &NotFoundError{Language: lang, Key: key}
}

func (t *Translator) GetOr(lang configuration.Language, key, fallback string) string {
	if v, err := t.Get(lang, key); err == nil {
		return v
	}
	return fallback
}

// Render returns the translated message for (lang, key) with `{name}`
// placeholders substituted by the values in vars (literal name match, no
// regex; the scanner accepts `[A-Za-z_][A-Za-z0-9_]*` between the braces).
//
// Behavior:
//   - vars == nil OR empty: no substitution, no warn (caller signaled
//     "I have no vars to provide"). Identical to Get on the resolution path.
//   - Placeholder present in the resolved string AND in vars: replaced.
//   - Placeholder present in the resolved string AND vars is non-empty BUT
//     missing the key: literal `{name}` left in the output AND
//     slog.Warn("translation.var.missing", ...) fires once per
//     (lang, key, placeholder) tuple via package-level dedup.
//   - Resolved key missing from the catalog: returns key as fallback AND
//     slog.Warn("translation.key.missing", ...) fires once per (lang, key).
//
// Get(lang, key) remains the raw lookup path — Render adds interpolation
// + warn-once on top.
func (t *Translator) Render(lang configuration.Language, key string, vars map[string]string) string {
	msg, err := t.Get(lang, key)
	if err != nil {
		warnTranslationKeyMissOnce(lang, key)
		msg = key
	}
	if len(vars) == 0 || !strings.Contains(msg, "{") {
		return msg
	}
	return interpolateMessage(msg, vars, func(placeholder string) {
		warnTranslationVarMissOnce(lang, key, placeholder)
	})
}

// Interpolate substitutes `{name}` placeholders in s using vars, with the same
// whitelist scanner Render uses. Intended for callers that already hold the
// translated string and only need the substitution step. Missing placeholders
// are left in the output verbatim WITHOUT firing the warn-once dedup —
// Interpolate has no (lang, key) context to attribute the miss to.
func Interpolate(s string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(s, "{") {
		return s
	}
	return interpolateMessage(s, vars, nil)
}

func (t *Translator) Has(lang configuration.Language, key string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if m, ok := t.translations[lang]; ok {
		_, has := m[key]
		return has
	}
	return false
}

type NotFoundError struct {
	Language configuration.Language
	Key      string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("translation: key %q not found for language %s", e.Key, e.Language)
}

// interpolateMessage scans s for `{name}` placeholders matching the whitelist
// `[A-Za-z_][A-Za-z0-9_]*` and replaces each match with vars[name]. Sequences
// that do not match the whitelist (`{`, `{1abc}`, `{not closed`) are written
// verbatim — the scanner never produces a malformed output. When a valid
// placeholder is not present in vars, the literal `{name}` is preserved AND
// the optional missing callback fires (used by Render to record per-(lang,key)
// warn-once dedup; Interpolate passes nil).
func interpolateMessage(s string, vars map[string]string, missing func(placeholder string)) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}
		j := i + 1
		if j >= len(s) || !isPlaceholderHead(s[j]) {
			b.WriteByte(c)
			i++
			continue
		}
		k := j + 1
		for k < len(s) && isPlaceholderTail(s[k]) {
			k++
		}
		if k >= len(s) || s[k] != '}' {
			b.WriteByte(c)
			i++
			continue
		}
		name := s[j:k]
		if val, ok := vars[name]; ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[i : k+1])
			if missing != nil {
				missing(name)
			}
		}
		i = k + 1
	}
	return b.String()
}

func isPlaceholderHead(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isPlaceholderTail(b byte) bool {
	return isPlaceholderHead(b) || (b >= '0' && b <= '9')
}

// warnTranslationKeyMissOnce emits slog.Warn the first time a (lang, key)
// tuple is observed as missing in the catalog. Subsequent observations of the
// same tuple within the same process are silent — surfacing the gap without
// log noise.
func warnTranslationKeyMissOnce(lang configuration.Language, key string) {
	langName := lang.String()
	composite := langName + "\x1f" + key
	if _, loaded := translationKeyMissOnce.LoadOrStore(composite, struct{}{}); loaded {
		return
	}
	slog.Warn("translation.key.missing",
		slog.String("lang", langName),
		slog.String("key", key))
}

// warnTranslationVarMissOnce emits slog.Warn the first time a
// (lang, key, placeholder) tuple is observed without a matching var entry.
// Same dedup mechanic as warnTranslationKeyMissOnce.
func warnTranslationVarMissOnce(lang configuration.Language, key, placeholder string) {
	langName := lang.String()
	composite := langName + "\x1f" + key + "\x1f" + placeholder
	if _, loaded := translationVarMissOnce.LoadOrStore(composite, struct{}{}); loaded {
		return
	}
	slog.Warn("translation.var.missing",
		slog.String("lang", langName),
		slog.String("key", key),
		slog.String("placeholder", placeholder))
}

// ResetWarnOnceForTesting clears the package-level warn-once dedup maps.
// Test-only — production code never calls this. Without the reset between
// tests, the first test to populate a (lang, key[, placeholder]) tuple would
// silence subsequent tests asserting the warn behavior on the same tuple.
func ResetWarnOnceForTesting() {
	translationKeyMissOnce.Range(func(k, _ any) bool {
		translationKeyMissOnce.Delete(k)
		return true
	})
	translationVarMissOnce.Range(func(k, _ any) bool {
		translationVarMissOnce.Delete(k)
		return true
	})
}

var (
	translationKeyMissOnce sync.Map // map[string]struct{} keyed by "<lang>\x1f<key>"
	translationVarMissOnce sync.Map // map[string]struct{} keyed by "<lang>\x1f<key>\x1f<placeholder>"
)

var (
	defaultOnce sync.Once
	defaultT    *Translator
)

func Default() *Translator {
	defaultOnce.Do(func() {
		defaultT = New()
		defaultT.Import(CorePTBR(), CoreENG(), CoreES(), CoreFR(), CoreDE(), CoreIT(), CoreNL())
	})
	return defaultT
}

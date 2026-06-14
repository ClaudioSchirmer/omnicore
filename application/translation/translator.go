package translation

import (
	"fmt"
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

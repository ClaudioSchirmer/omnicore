package configuration

import "github.com/ClaudioSchirmer/omnicore/domain"

type Language int

// Explicit values (never bare iota): reordering this block must never change a
// persisted number.
const (
	LangUnknown Language = 0
	LangPTBR    Language = 1
	LangENG     Language = 2
	LangES      Language = 3
	LangFR      Language = 4
	LangDE      Language = 5
	LangIT      Language = 6
	LangNL      Language = 7
)

// Values is the closed set (Unknown excluded); the framework validates
// membership against it — Language writes no IsValid.
func (l Language) Value() int         { return int(l) }
func (l Language) Values() []Language { return AllLanguages() }

func (l Language) UnknownNotification() domain.Notification {
	return InvalidLanguageDomainNotification{}
}

func (l Language) String() string {
	switch l {
	case LangPTBR:
		return "PT_BR"
	case LangENG:
		return "ENG"
	case LangES:
		return "ES"
	case LangFR:
		return "FR"
	case LangDE:
		return "DE"
	case LangIT:
		return "IT"
	case LangNL:
		return "NL"
	default:
		return "UNKNOWN"
	}
}

// AllLanguages returns the slice with all known Language values in
// declaration order (excluding LangUnknown). Used by parsers that need
// to iterate over the enum (web middleware, translation validation).
func AllLanguages() []Language {
	return []Language{LangPTBR, LangENG, LangES, LangFR, LangDE, LangIT, LangNL}
}

// HTTPPrefix returns the corresponding Accept-Language prefix. Empty if
// there is no prefix defined for this Language (default in the parser).
func (l Language) HTTPPrefix() string {
	switch l {
	case LangPTBR:
		return "pt"
	case LangENG:
		return "en"
	case LangES:
		return "es"
	case LangFR:
		return "fr"
	case LangDE:
		return "de"
	case LangIT:
		return "it"
	case LangNL:
		return "nl"
	}
	return ""
}

type InvalidLanguageDomainNotification struct {
	domain.ApplicationNotificationBase
}

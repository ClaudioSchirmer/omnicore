package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type Module interface {
	Language() configuration.Language
	Translations() map[string]string
}

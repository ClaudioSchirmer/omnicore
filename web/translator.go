package web

import (
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// SetTranslator registers the framework Translator with the web package for
// the few code paths that need to translate a notification message without
// going through the Pipeline:
//
//   - RespondWithInternalServerError (the canonical 500 fallback used by
//     RespondFromResult on the Exception branch, and by openapi/ in spec
//     assembly bail-outs).
//   - PermissionGate's 403 envelope (Mount/MountRaw runtime gate — runs
//     before the handler and has no Pipeline.Result to thread through).
//
// Called by bootstrap.Run with deps.Translator before features mount.
// Idempotent and concurrent-safe; in practice called once per process at
// boot. When nil (consumer using bootstrap.Build + bootstrap.Serve manually
// and forgetting this call), the standalone paths fall back to the English
// default — the wire envelope still emits, only the translated message is
// missing.
func SetTranslator(tr *translation.Translator) {
	translatorMu.Lock()
	defer translatorMu.Unlock()
	registeredTr = tr
}

func registeredTranslator() *translation.Translator {
	translatorMu.RLock()
	defer translatorMu.RUnlock()
	return registeredTr
}

var (
	translatorMu sync.RWMutex
	registeredTr *translation.Translator
)

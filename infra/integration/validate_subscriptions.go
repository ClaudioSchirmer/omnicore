package integration

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateSubscriptionsCovered enforces that every (sourceKey, eventKey) the
// YAML declares under integration.subscribes has a matching Receiver
// registered via MountReceivers (reg.From(source).On(eventKey, …)). The
// ConsumerPool only spins goroutines for REGISTERED receivers, so a
// declared-but-unregistered subscription consumes nothing — every message on
// that topic is silently dropped with no error. The inverse direction
// (a registered receiver with no YAML coordinate) is already caught by
// Receiver.resolveAgainstYAML; this closes the other half so the two sides
// of the subscribe contract must agree.
//
// Both inputs are known at boot: the parsed Config and the populated Registry
// after MountReceivers. Returns an aggregated error (boot-abort surface,
// matching the integration package's error-return posture) listing every
// uncovered coordinate. Returns nil when cfg is nil or every declared event
// is covered.
func ValidateSubscriptionsCovered(cfg *Config, receivers []*Receiver) error {
	if cfg == nil {
		return nil
	}
	covered := make(map[string]struct{}, len(receivers))
	for _, r := range receivers {
		covered[subscriptionKey(r.sourceKey, r.eventKey)] = struct{}{}
	}
	var orphans []string
	for srcKey, src := range cfg.Subscribes {
		for evKey := range src.Events {
			if _, ok := covered[subscriptionKey(srcKey, evKey)]; !ok {
				orphans = append(orphans,
					fmt.Sprintf("integration.subscribes.%s.events.%s", srcKey, evKey))
			}
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)
	return fmt.Errorf(
		"integration: %d declared subscription(s) have no registered receiver — "+
			"MountReceivers must call reg.From(source).On(eventKey, sample, handler) for each, "+
			"otherwise no goroutine consumes the topic and every message is silently dropped:\n  - %s",
		len(orphans), strings.Join(orphans, "\n  - "))
}

// subscriptionKey joins a (sourceKey, eventKey) pair with a NUL separator so
// the composite cannot collide with a key built from differently-split parts.
func subscriptionKey(sourceKey, eventKey string) string {
	return sourceKey + "\x00" + eventKey
}

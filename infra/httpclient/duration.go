package httpclient

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration with a YAML decoder that accepts the standard
// Go duration notation (e.g. "30s", "5m", "1h"). Using a dedicated type keeps
// the YAML schema human-readable while letting the Go code work with plain
// time.Duration via ToTime.
//
// The zero value is treated as "not set" by applyDefaults so the cascade can
// distinguish an explicit "0s" from a missing field if it ever needs to. In
// practice the loader requires positive durations; Validate rejects negatives.
type Duration time.Duration

// UnmarshalYAML decodes a string node such as "30s" into a Duration. Returns a
// descriptive error when the input is not a valid Go duration string so the
// boot error points the operator at the offending YAML location.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration: decode yaml node at line %d: %w", node.Line, err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: parse %q at line %d: %w", s, node.Line, err)
	}
	*d = Duration(parsed)
	return nil
}

// ToTime returns the wrapped time.Duration for use with stdlib APIs.
func (d Duration) ToTime() time.Duration {
	return time.Duration(d)
}

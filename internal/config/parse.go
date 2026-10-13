package config

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Helpers that validate values decoded by BurntSushi/toml into
// map[string]any (tables), []any (arrays), int64, float64, string, bool.

func errorf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

func asTable(value any, label string) (map[string]any, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, errorf("%s must be a table", label)
	}
	return table, nil
}

// optTable returns nil, nil when key is absent.
func optTable(parent map[string]any, key, label string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok {
		return nil, nil
	}
	return asTable(value, label)
}

func checkKeys(table map[string]any, label string, allowed ...string) error {
	allow := map[string]bool{}
	for _, key := range allowed {
		allow[key] = true
	}
	var unknown []string
	for key := range table {
		if !allow[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return errorf("unknown %s setting %q", label, unknown[0])
}

// optString returns nil when absent; present values must be non-blank
// strings.
func optString(table map[string]any, key, label string) (*string, error) {
	value, ok := table[key]
	if !ok {
		return nil, nil
	}
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil, errorf("%s.%s must be a non-empty string", label, key)
	}
	return &s, nil
}

func optInt(table map[string]any, key, label string) (*int, error) {
	value, ok := table[key]
	if !ok {
		return nil, nil
	}
	n, ok := value.(int64)
	if !ok || n > math.MaxInt32 || n < math.MinInt32 {
		return nil, errorf("%s.%s must be an integer", label, key)
	}
	i := int(n)
	return &i, nil
}

// optSeconds accepts an integer or float number of seconds.
func optSeconds(table map[string]any, key, label string) (*time.Duration, error) {
	value, ok := table[key]
	if !ok {
		return nil, nil
	}
	var seconds float64
	switch v := value.(type) {
	case int64:
		seconds = float64(v)
	case float64:
		seconds = v
	default:
		return nil, errorf("%s.%s must be a number of seconds", label, key)
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 86400 {
		return nil, errorf("%s.%s must be between 0 and 86400 seconds", label, key)
	}
	d := time.Duration(seconds * float64(time.Second))
	return &d, nil
}

// optStringList returns (nil, false) when absent.
func optStringList(table map[string]any, key, label string) ([]string, bool, error) {
	value, ok := table[key]
	if !ok {
		return nil, false, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, true, errorf("%s.%s must be a list of non-empty strings", label, key)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, true, errorf("%s.%s must be a list of non-empty strings", label, key)
		}
		out = append(out, s)
	}
	return out, true, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

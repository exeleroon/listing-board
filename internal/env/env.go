// Package env reads process configuration shared by the API and the worker.
package env

import (
	"fmt"
	"os"
	"time"
)

// String returns the variable, or fallback when it is unset or empty.
func String(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Duration parses the variable as a Go duration such as 2s or 500ms. An
// unset or empty variable yields fallback; a malformed one is an error so a
// typo does not silently turn into the default.
func Duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return d, nil
}

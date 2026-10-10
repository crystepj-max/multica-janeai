package requestkey

import (
	"errors"
	"strings"
)

const MaxIdempotencyKeyBytes = 255

var (
	ErrIdempotencyKeyRequired = errors.New("Idempotency-Key is required")
	ErrIdempotencyKeyTooLong  = errors.New("Idempotency-Key is too long")
)

// ParseIdempotencyKey normalizes an HTTP idempotency key and enforces the
// shared Public API byte limit before a write reaches a service.
func ParseIdempotencyKey(value string) (string, error) {
	key := strings.TrimSpace(value)
	if key == "" {
		return "", ErrIdempotencyKeyRequired
	}
	if len(key) > MaxIdempotencyKeyBytes {
		return "", ErrIdempotencyKeyTooLong
	}
	return key, nil
}

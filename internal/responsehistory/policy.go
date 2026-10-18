// Package responsehistory defines the shared Responses transcript storage policy.
package responsehistory

import (
	"fmt"
	"time"
)

// Policy limits serialized transcript payloads, not allocator or Redis overhead.
// Every field must be positive; omitted fields are supplied by Default.
type Policy struct {
	TTLSeconds   int64 `json:"ttl_seconds"`
	MaxEntries   int   `json:"max_entries"`
	MaxStorageMB int64 `json:"max_storage_mb"`
}

func Default() Policy {
	return Policy{TTLSeconds: 3600, MaxEntries: 1024, MaxStorageMB: 64}
}

func (p Policy) Validate() error {
	if p.TTLSeconds < 1 || p.TTLSeconds > 3153600000 {
		return fmt.Errorf("response_history.ttl_seconds must be 1-3153600000 seconds")
	}
	if p.MaxEntries < 1 || p.MaxEntries > 1000000000 {
		return fmt.Errorf("response_history.max_entries must be 1-1000000000")
	}
	if p.MaxStorageMB < 1 || p.MaxStorageMB > 1000000000 {
		return fmt.Errorf("response_history.max_storage_mb must be 1-1000000000 MB")
	}
	return nil
}

func (p Policy) TTL() time.Duration { return time.Duration(p.TTLSeconds) * time.Second }
func (p Policy) MaxBytes() int64    { return p.MaxStorageMB * 1_000_000 }

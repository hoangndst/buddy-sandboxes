package workspace

import (
	"testing"
	"time"
)

func TestDefaultLimits(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxEntries != 10_000 || limits.MaxTotalBytes != 100<<20 || limits.MaxFileBytes != 20<<20 ||
		limits.MaxPathBytes != 4<<10 || limits.TransferTimeout != 5*time.Minute {
		t.Fatalf("unexpected limits: %+v", limits)
	}
}

package main

import (
	"strings"
	"testing"
	"time"
)

// The build-object fallback must still satisfy peipkg's §3.3.4 validation:
// a non-empty farm_id and source_ref, and an RFC3339 timestamp in UTC (the
// validator rejects any timestamp that does not end in Z).
func TestUnanchoredProvenanceIsValid(t *testing.T) {
	b := unanchoredProvenance()
	if b.FarmID != "local" {
		t.Errorf("FarmID = %q, want %q", b.FarmID, "local")
	}
	if b.SourceRef != "unknown" {
		t.Errorf("SourceRef = %q, want %q", b.SourceRef, "unknown")
	}
	if !strings.HasSuffix(b.Timestamp, "Z") {
		t.Errorf("Timestamp %q must be UTC (end with Z)", b.Timestamp)
	}
	if _, err := time.Parse(time.RFC3339, b.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not RFC3339: %v", b.Timestamp, err)
	}
}

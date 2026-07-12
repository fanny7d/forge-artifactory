package clock

import (
	"testing"
	"time"
)

func TestFixedReturnsConfiguredTime(t *testing.T) {
	want := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	got := (Fixed{Time: want}).Now()
	if !got.Equal(want) {
		t.Fatalf("Now() = %s, want %s", got, want)
	}
}

func TestSystemReturnsUTC(t *testing.T) {
	got := (System{}).Now()
	if got.Location() != time.UTC {
		t.Fatalf("Now() location = %s, want UTC", got.Location())
	}
}

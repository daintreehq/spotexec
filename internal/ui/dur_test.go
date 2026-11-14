package ui

import (
	"testing"
	"time"
)

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		450 * time.Millisecond:                 "450ms",
		4100 * time.Millisecond:                "4.1s",
		time.Minute:                            "1m",
		75 * time.Second:                       "1m15s",
		6*time.Minute + 50*time.Second:         "6m50s",
		10*time.Minute + 30*time.Second:        "10m30s",
		time.Hour + 20*time.Second:             "1h0m20s",
		6*time.Minute + 49600*time.Millisecond: "6m50s",
		2 * time.Hour:                          "2h0m",
	} {
		if got := Dur(d); got != want {
			t.Errorf("Dur(%v) = %q, want %q", d, got, want)
		}
	}
}

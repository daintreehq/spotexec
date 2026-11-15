package protocol

import "testing"

func TestHeartbeatPressure(t *testing.T) {
	for _, c := range []struct {
		hb   Heartbeat
		want bool
	}{
		{Heartbeat{}, false}, // an older helper's empty heartbeat
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 8000}, false},
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 801}, false}, // just over 5%
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 800}, false}, // exactly 5%
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 799}, true},
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 0}, true},
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 8000, PSIFull10: 9.9}, false},
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 8000, PSIFull10: 10}, true},
		{Heartbeat{MemTotalMB: 16000, MemAvailMB: 8000, PSIFull10: 25}, true},
	} {
		if got := c.hb.Pressure() != ""; got != c.want {
			t.Errorf("%+v: pressure %v (%q), want %v", c.hb, got, c.hb.Pressure(), c.want)
		}
	}
}

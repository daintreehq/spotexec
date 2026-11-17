package protocol

import "testing"

func TestAdmitLimit(t *testing.T) {
	for slots, want := range map[int]int{1: 2, 2: 4, 21: 42, 22: 44, 30: 44, MaxSlots: MaxSlots} {
		if got := AdmitLimit(slots); got != want {
			t.Errorf("AdmitLimit(%d) = %d, want %d", slots, got, want)
		}
	}
}

// A busy runner takes an overflow request only while some of its jobs
// prepare and fewer than its slots run commands; an exclusive run (one
// helper holding every token) counts once.
func TestOverflowable(t *testing.T) {
	prep := Holder{Token: 3, Phased: true, HelperPID: 30}
	run := Holder{Token: 1, Phased: true, Phase: PhaseRun, HelperPID: 10}
	wait := Holder{Token: 2, Phased: true, Phase: PhaseRunWait, HelperPID: 20}
	old := Holder{Token: 4, HelperPID: 40}
	for _, c := range []struct {
		hs   []Holder
		want bool
	}{
		{[]Holder{run, prep}, true},
		{[]Holder{run, wait, prep}, false},
		{[]Holder{run, old, prep}, false},
		{[]Holder{run, wait}, false},
		{[]Holder{prep, {Token: 5, Phased: true, HelperPID: 50}}, true},
	} {
		if got := Overflowable(c.hs, 2); got != c.want {
			t.Errorf("Overflowable(%+v, 2) = %v, want %v", c.hs, got, c.want)
		}
	}
	excl := []Holder{{Token: 1, HelperPID: 7}, {Token: 2, HelperPID: 7}, {Token: 3, HelperPID: 7}}
	if n := RunningJobs(excl); n != 1 {
		t.Fatalf("an exclusive run holding three tokens counts as %d jobs", n)
	}
	if n := RunningJobs([]Holder{{Token: 1}, {Token: 2}}); n != 2 {
		t.Fatalf("two tokens held without records count as %d jobs", n)
	}
}

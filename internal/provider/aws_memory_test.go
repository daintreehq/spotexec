package provider

import (
	"fmt"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// A workload is expected to need the most any of its latest runs did,
// within memFor; older runs and runs past memSamples stop counting, so a
// workload that needs less (or one recorded at the pool's size after an
// OOM kill) comes down once it has run that many times.
func TestExpectedMemory(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st := &placementState{}
	if got := expectedMemory(st, "tc", now); got != 0 {
		t.Fatalf("unmeasured: %d", got)
	}
	recordMemory(st, "tc", 29000, now) // OOM killed: the pool's cap
	for i, mb := range []int64{7000, 8000, 7500, 7600} {
		recordMemory(st, "tc", mb, now.Add(time.Duration(i+1)*time.Minute))
	}
	if got := expectedMemory(st, "tc", now.Add(time.Hour)); got != 29000 {
		t.Fatalf("with the OOM kill among the last %d: %d", memSamples, got)
	}
	recordMemory(st, "tc", 7700, now.Add(10*time.Minute))
	if got := expectedMemory(st, "tc", now.Add(time.Hour)); got != 8000 {
		t.Fatalf("after %d more runs: %d, want 8000", memSamples, got)
	}
	if got := expectedMemory(st, "other", now); got != 0 {
		t.Fatalf("another workload: %d", got)
	}
	if got := expectedMemory(st, "tc", now.Add(memFor+time.Hour)); got != 0 {
		t.Fatalf("expired: %d", got)
	}
	// Recording forgets what expired, and the least recently run
	// workloads past memWorkloads.
	later := now.Add(memFor + time.Hour)
	for i := 0; i <= memWorkloads; i++ {
		recordMemory(st, fmt.Sprintf("w%03d", i), 100, later.Add(time.Duration(i)*time.Second))
	}
	if _, ok := st.Memory["tc"]; ok {
		t.Fatal("an expired workload was kept")
	}
	if _, ok := st.Memory["w000"]; ok || len(st.Memory) != memWorkloads {
		t.Fatalf("%d workloads kept, w000 among them: %v", len(st.Memory), ok)
	}
}

// Only Spot runners (spotexec's VMs, which have the memory pool) keep
// peaks; the state round-trips through placement.json.
func TestRecordRunMemory(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	ssh := &config.Runner{Name: "box", Type: config.TypeSSH}
	if err := RecordRunMemory(ssh, "w", 1000); err != nil || RunMemory(ssh, "w") != 0 {
		t.Fatal("an SSH runner kept a peak")
	}
	spot := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	for _, mb := range []int64{1000, 3000, 0, 2000} {
		if err := RecordRunMemory(spot, "w", mb); err != nil {
			t.Fatal(err)
		}
	}
	if got := RunMemory(spot, "w"); got != 3000 {
		t.Fatalf("expected %d MiB, want 3000", got)
	}
}

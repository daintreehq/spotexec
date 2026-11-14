package helper

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLifetimeLeft(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if _, near := lifetimeLeft(dir, now); near {
		t.Fatal("no lifetime files (a static runner): never near")
	}
	write := func(name string, v int64) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.FormatInt(v, 10)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("max_lifetime", int64((12 * time.Hour).Seconds()))
	write("created_at", now.Add(-11*time.Hour).Unix())
	if left, near := lifetimeLeft(dir, now); near || left < 59*time.Minute {
		t.Fatalf("an hour left: %s %v", left, near)
	}
	write("created_at", now.Add(-12*time.Hour+10*time.Minute).Unix())
	if left, near := lifetimeLeft(dir, now); !near || left > 11*time.Minute {
		t.Fatalf("ten minutes left: %s %v", left, near)
	}
	// A short lifetime drains a quarter ahead.
	write("max_lifetime", 80)
	write("created_at", now.Add(-50*time.Second).Unix())
	if _, near := lifetimeLeft(dir, now); near {
		t.Fatal("30s of 80s left is outside the 20s window")
	}
	write("created_at", now.Add(-65*time.Second).Unix())
	if _, near := lifetimeLeft(dir, now); !near {
		t.Fatal("15s of 80s left is inside the 20s window")
	}
}

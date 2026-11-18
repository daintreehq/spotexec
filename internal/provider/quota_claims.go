package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Overlapping launches of a runner each read the region's quota usage
// before sending, so none of them sees the others' VMs. Each admitted
// standard Spot launch records a claim on its vCPUs until listings show
// the instance it made, and the pre-launch check counts the claims whose
// instance its usage doesn't already list.
type quotaClaim struct {
	Region string    `json:"region"`
	VCPUs  int       `json:"vcpus"`
	ID     string    `json:"id,omitempty"` // the instance, once launched
	Until  time.Time `json:"until"`
}

const (
	quotaClaimsFile = "quota-claims.json"
	quotaClaimsLock = "quota-claims.lock"
	// claimSending bounds a claim while its request is out (with the
	// call's own retries), so one left by a process that died ends.
	claimSending = 10 * time.Minute
	// claimListLag keeps a claim after its launch (or a lost response)
	// until listings can show the instance.
	claimListLag = 2 * time.Minute
	// claimsWait bounds a wait for the claims lock, which is only ever
	// held for a read and a write.
	claimsWait = 30 * time.Second
)

// withClaims runs fn on the runner's live quota claims under their lock
// and saves them. Without a state directory there are none to keep. An
// error before fn runs (the lock, the file) means fn didn't run.
func (a *AWSSpot) withClaims(fn func(cs map[string]quotaClaim)) error {
	return a.withLedger(quotaClaimsLock, quotaClaimsFile, fn)
}

// withLedger is withClaims for any short-lived ledger of claims kept in
// file under lock (expired entries are dropped first).
func (a *AWSSpot) withLedger(lock, file string, fn func(cs map[string]quotaClaim)) error {
	if a.dir == "" {
		fn(map[string]quotaClaim{})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimsWait)
	defer cancel()
	release, err := waitLockFile(ctx, a.lockPath(lock), syscall.LOCK_EX, nil)
	if err != nil {
		return err
	}
	defer release()
	path := a.lockPath(file)
	var cs map[string]quotaClaim
	switch b, err := os.ReadFile(path); {
	case err == nil:
		_ = json.Unmarshal(b, &cs) // unreadable: claims are short-lived, start over
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if cs == nil {
		cs = map[string]quotaClaim{}
	}
	now := time.Now()
	for k, c := range cs {
		if now.After(c.Until) {
			delete(cs, k)
		}
	}
	fn(cs)
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic replaces path with b through a temporary file of its own,
// so concurrent writers never share one.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(f.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return werr
}

// claimed is u with the claims in its region that it doesn't list yet
// counted as this machine's.
func (u QuotaUsage) claimed(cs map[string]quotaClaim) QuotaUsage {
	for _, c := range cs {
		if c.Region == u.Region && (c.ID == "" || !u.ids[c.ID]) {
			u.Used += c.VCPUs
			u.Mine += c.VCPUs
		}
	}
	return u
}

// admitClaimed is the pre-launch check for a standard Spot launch of need
// vCPUs against u and the runner's claims. When it fits, it claims them,
// and the returned settle records how the launch went.
func (a *AWSSpot) admitClaimed(u QuotaUsage, need int) (*QuotaError, func(ec2Instance, error)) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	key := hex.EncodeToString(b[:])
	var qe *QuotaError
	ran := false
	_ = a.withClaims(func(cs map[string]quotaClaim) {
		ran = true
		if qe = a.admitSpot(u.claimed(cs), need); qe == nil {
			cs[key] = quotaClaim{Region: u.Region, VCPUs: need, Until: time.Now().Add(claimSending)}
		}
	})
	switch {
	case !ran:
		// No claims to go by: the usage alone decides, as before.
		return a.admitSpot(u, need), func(ec2Instance, error) {}
	case qe != nil:
		return qe, nil // refused even if saving failed
	}
	return nil, func(inst ec2Instance, err error) {
		_ = a.withClaims(func(cs map[string]quotaClaim) {
			// Recorded afresh: a request that outlasted claimSending may
			// have had its claim pruned.
			c := quotaClaim{Region: u.Region, VCPUs: need, Until: time.Now().Add(claimListLag)}
			switch {
			case err == nil:
				c.ID = inst.InstanceID
				cs[key] = c
			case errors.Is(err, ErrLaunchAmbiguous):
				cs[key] = c // it may have launched
			default:
				delete(cs, key)
			}
		})
	}
}

// releaseClaims drops the claims on instances in region that were just
// terminated, so their vCPUs are free once listings stop showing them.
func (a *AWSSpot) releaseClaims(ids []string) {
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	_ = a.withClaims(func(cs map[string]quotaClaim) {
		for k, c := range cs {
			if c.Region == a.r.Region && c.ID != "" && gone[c.ID] {
				delete(cs, k)
			}
		}
	})
}

// Every fleet instance this machine launches is noted (a claim with no
// vCPUs, keyed by its ID) until tag-filtered listings can show it, so a
// Down right after the launch looks it up by ID instead of missing it.
const (
	launchedFile = "launched.json"
	launchedLock = "launched.lock"
)

// noteLaunched records instance id, just launched in the region.
func (a *AWSSpot) noteLaunched(id string) error {
	return a.withLedger(launchedLock, launchedFile, func(cs map[string]quotaClaim) {
		cs[id] = quotaClaim{Region: a.r.Region, ID: id, Until: time.Now().Add(claimListLag)}
	})
}

// recentLaunches lists the instances noted in the region whose listing
// may still lag.
func (a *AWSSpot) recentLaunches() ([]string, error) {
	var ids []string
	err := a.withLedger(launchedLock, launchedFile, func(cs map[string]quotaClaim) {
		for _, c := range cs {
			if c.Region == a.r.Region && c.ID != "" {
				ids = append(ids, c.ID)
			}
		}
	})
	sort.Strings(ids)
	return ids, err
}

// forgetLaunched drops the notes of instances that were just terminated.
func (a *AWSSpot) forgetLaunched(ids []string) {
	_ = a.withLedger(launchedLock, launchedFile, func(cs map[string]quotaClaim) {
		for _, id := range ids {
			if c, ok := cs[id]; ok && c.Region == a.r.Region {
				delete(cs, id)
			}
		}
	})
}

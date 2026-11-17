package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	// claimSending bounds a claim while its request is out, so one left by
	// a process that died ends.
	claimSending = 5 * time.Minute
	// claimListLag keeps a claim after its launch (or a lost response)
	// until listings can show the instance.
	claimListLag = 2 * time.Minute
)

// withClaims runs fn on the runner's live quota claims under their lock
// and saves them. Without a state directory there are none to keep.
func (a *AWSSpot) withClaims(fn func(cs map[string]quotaClaim)) error {
	if a.dir == "" {
		fn(map[string]quotaClaim{})
		return nil
	}
	release, err := waitLockFile(context.Background(), a.lockPath(quotaClaimsLock), syscall.LOCK_EX, nil)
	if err != nil {
		return err
	}
	defer release()
	path := a.lockPath(quotaClaimsFile)
	cs := map[string]quotaClaim{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cs) // unreadable: claims are short-lived, start over
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
	err := a.withClaims(func(cs map[string]quotaClaim) {
		if qe = a.admitSpot(u.claimed(cs), need); qe == nil {
			cs[key] = quotaClaim{Region: u.Region, VCPUs: need, Until: time.Now().Add(claimSending)}
		}
	})
	if err != nil {
		// No claims to go by: the usage alone decides, as before.
		return a.admitSpot(u, need), func(ec2Instance, error) {}
	}
	if qe != nil {
		return qe, nil
	}
	return nil, func(inst ec2Instance, err error) {
		_ = a.withClaims(func(cs map[string]quotaClaim) {
			c, ok := cs[key]
			switch {
			case !ok:
			case err == nil:
				c.ID, c.Until = inst.InstanceID, time.Now().Add(claimListLag)
				cs[key] = c
			case errors.Is(err, ErrLaunchAmbiguous):
				c.Until = time.Now().Add(claimListLag) // it may have launched
				cs[key] = c
			default:
				delete(cs, key)
			}
		})
	}
}

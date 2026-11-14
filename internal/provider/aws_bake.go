package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/daintreehq/spotexec/internal/config"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Baked images ("build phase"): `spotexec image bake` runs a runner's
// bootstrap once on an on-demand builder, installs Docker, optionally
// pre-pulls images, sanitizes the machine and snapshots it into an AMI.
// Fleet launches then use the newest bake whose bootstrap hash matches and
// skip the bootstrap, cutting cold boot to roughly the bare VM boot. The
// per-instance user-data (watchdog, SSH key, timeouts) still runs on every
// launch, because the bake is cloud-init-cleaned.

const tagBakeHash = "spotexec-bootstrap-hash"

// BakeHash identifies what a bake contains; changing the bootstrap scripts,
// the login user or the architecture invalidates old bakes. The base AMI ID
// is deliberately excluded (Canonical refreshes it every few weeks).
func (a *AWSSpot) BakeHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%s\x00%s", a.r.Arch, a.r.User, a.r.BootstrapScript, a.r.UserBootstrap)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (a *AWSSpot) bakeRootScript(prepull []string) string {
	var b strings.Builder
	// In a subshell, so an `exit 0` in the user's script can't skip the rest.
	// Not followed by ||: that would disable -e inside it. ":" keeps an
	// empty script valid.
	b.WriteString("(\n:\n" + a.r.BootstrapScript + "\n)\n")
	b.WriteString("\n# spotexec bake: docker for project containers\n")
	b.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("command -v docker >/dev/null 2>&1 || { apt-get update -o DPkg::Lock::Timeout=300 -qq && apt-get install -y -qq -o DPkg::Lock::Timeout=300 docker.io docker-buildx zstd; }\n")
	b.WriteString("systemctl enable --now docker\n")
	b.WriteString("usermod -aG docker " + a.r.User + "\n")
	for _, img := range prepull {
		if imageRefOK(img) {
			// A failed pull fails the bootstrap, and with it the bake.
			b.WriteString("docker pull -q '" + img + "'\n")
		}
	}
	return b.String()
}

// ImageRefOK validates a container image reference.
func ImageRefOK(s string) bool { return imageRefOK(s) }

func imageRefOK(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/:@-", r)) {
			return false
		}
	}
	return true
}

type ec2Image struct {
	ID         string `xml:"imageId"`
	Name       string `xml:"name"`
	State      string `xml:"imageState"`
	Created    string `xml:"creationDate"`
	RootDevice string `xml:"rootDeviceName"`
	Devices    []struct {
		Name string `xml:"deviceName"`
		Size int    `xml:"ebs>volumeSize"`
	} `xml:"blockDeviceMapping>item"`
}

// rootSize is the image's root volume size in GiB (0 if unknown).
func (i ec2Image) rootSize() int {
	for _, d := range i.Devices {
		if d.Name == i.RootDevice {
			return d.Size
		}
	}
	return 0
}

func (a *AWSSpot) ec2Arch() string {
	if a.r.Arch == "arm64" {
		return "arm64"
	}
	return "x86_64"
}

// listBakes returns this runner's available bakes (newest first); with
// matchHash only those built from the current bootstrap.
func (a *AWSSpot) listBakes(ctx context.Context, matchHash bool) ([]ec2Image, error) {
	return a.listBakesIn(ctx, matchHash, "available")
}

func (a *AWSSpot) listBakesIn(ctx context.Context, matchHash bool, states ...string) ([]ec2Image, error) {
	owner, err := a.ownerTag()
	if err != nil {
		return nil, err
	}
	p := url.Values{}
	p.Set("Owner.1", "self")
	p.Set("Filter.1.Name", "state")
	for i, st := range states {
		p.Set("Filter.1.Value."+strconv.Itoa(i+1), st)
	}
	// Architecture only matters for picking a bake to boot; listing and
	// clearing must also see bakes from a previous arch setting.
	p.Set("Filter.2.Name", "architecture")
	p.Set("Filter.2.Value.1", "x86_64")
	p.Set("Filter.2.Value.2", "arm64")
	if matchHash {
		p.Set("Filter.2.Value.1", a.ec2Arch())
		p.Del("Filter.2.Value.2")
	}
	p.Set("Filter.3.Name", "tag:"+tagRunner)
	p.Set("Filter.3.Value.1", a.r.Name)
	p.Set("Filter.4.Name", "tag:"+tagOwner)
	p.Set("Filter.4.Value.1", owner)
	if matchHash {
		p.Set("Filter.5.Name", "tag:"+tagBakeHash)
		p.Set("Filter.5.Value.1", a.BakeHash())
	}
	b, err := a.api.ec2(ctx, "DescribeImages", p)
	if err != nil {
		return nil, err
	}
	var v struct {
		Images []ec2Image `xml:"imagesSet>item"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	sort.Slice(v.Images, func(i, j int) bool { return v.Images[i].Created > v.Images[j].Created })
	return v.Images, nil
}

// findBake returns the newest bake matching the current bootstrap, its
// root device and root volume size (launches can't use a smaller volume).
func (a *AWSSpot) findBake(ctx context.Context) (string, string, int, time.Time, error) {
	imgs, err := a.listBakes(ctx, true)
	if err != nil || len(imgs) == 0 {
		return "", "", 0, time.Time{}, err
	}
	created, _ := time.Parse(time.RFC3339, imgs[0].Created)
	return imgs[0].ID, imgs[0].RootDevice, imgs[0].rootSize(), created, nil
}

// SanitizeScript prepares a bake builder for imaging: everything instance-
// specific goes, so each VM launched from the image initializes fresh
// (cloud-init re-runs the per-instance user-data, new host keys and
// machine-id, a new watchdog with the runner's current timeouts).
func (a *AWSSpot) SanitizeScript() string {
	u := a.r.User
	return `set -e
# The transient spotexec-maxlife timer (under /run, not imaged) stays armed:
# it still terminates this builder if the bake is abandoned.
sudo -n systemctl stop spotexec-watchdog.timer spotexec-watchdog.service 2>/dev/null || true
sudo -n systemctl disable spotexec-watchdog.timer 2>/dev/null || true
sudo -n rm -f /etc/systemd/system/spotexec-watchdog.timer /etc/systemd/system/spotexec-watchdog.service /usr/local/sbin/spotexec-watchdog
sudo -n systemctl daemon-reload
sudo -n systemctl stop docker.socket docker.service 2>/dev/null || true
sudo -n apt-get clean
sudo -n rm -rf /var/lib/apt/lists/* /var/tmp/spotexec /var/log/spotexec-bootstrap.log /home/` + u + `/.cache/spotexec /root/.ssh/authorized_keys
sudo -n rm -f /etc/ssh/ssh_host_*
sudo -n cloud-init clean --logs --seed --machine-id 2>/dev/null || { sudo -n cloud-init clean --logs --seed; sudo -n truncate -s 0 /etc/machine-id; }
sudo -n journalctl --rotate --vacuum-time=1s >/dev/null 2>&1 || true
sudo -n truncate -s 0 /var/log/wtmp /var/log/lastlog 2>/dev/null || true
sudo -n fstrim -av >/dev/null 2>&1 || true
sync
rm -f /home/` + u + `/.ssh/authorized_keys
echo sanitized`
}

// BakeLaunch starts the builder (on-demand, tagged so the fleet never uses it).
func (a *AWSSpot) BakeLaunch(ctx context.Context, log Logger, prepull []string) (Instance, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	inst, err := a.launchWith(ctx, log, launchSpec{bake: true, prepull: prepull})
	if err != nil {
		return Instance{}, err
	}
	return a.instance(inst), nil
}

// BakeImage stops the sanitized builder, images it, waits until the image
// is available, terminates the builder and prunes older bakes (keeping two,
// so instances launching from the previous one are not disturbed). On any
// failure after CreateImage was sent, the half-made image and its snapshots
// are removed too (an ambiguous CreateImage is reconciled by its unique
// name).
func (a *AWSSpot) BakeImage(ctx context.Context, builderID string, log Logger) (ami string, err error) {
	name := fmt.Sprintf("spotexec-%s-%s-%d", a.r.Name, a.BakeHash(), time.Now().Unix())
	defer func() {
		clean := context.WithoutCancel(ctx)
		if err != nil {
			if ami == "" {
				ami = a.imageByName(clean, name)
			}
			if ami != "" {
				if derr := a.deregister(clean, ami, log); derr != nil {
					err = fmt.Errorf("%w; ALSO could not remove partial image %s: %v (run `spotexec image clear %s`)", err, ami, derr, a.r.Name)
				}
				ami = ""
			}
		}
		if terr := a.terminateAny(clean, builderID); terr != nil {
			msg := fmt.Errorf("could not terminate bake builder %s: %v; terminate it in the EC2 console", builderID, terr)
			if err == nil {
				err = msg
			} else {
				err = fmt.Errorf("%w; %v", err, msg)
			}
		}
	}()
	p := url.Values{}
	p.Set("InstanceId.1", builderID)
	if _, err := a.api.ec2(ctx, "StopInstances", p); err != nil {
		return "", fmt.Errorf("stopping builder: %w", err)
	}
	log("stopping builder %s", builderID)
	if err := a.waitInstanceState(ctx, builderID, "stopped", 10*time.Minute); err != nil {
		return "", err
	}
	owner, err := a.ownerTag()
	if err != nil {
		return "", err
	}
	c := url.Values{}
	c.Set("InstanceId", builderID)
	c.Set("Name", name)
	c.Set("Description", "spotexec baked runner image for "+a.r.Name)
	tags := [][2]string{{tagRunner, a.r.Name}, {tagOwner, owner}, {tagBakeHash, a.BakeHash()}, {"Name", name}}
	for i, rt := range []string{"image", "snapshot"} {
		pfx := "TagSpecification." + strconv.Itoa(i+1)
		c.Set(pfx+".ResourceType", rt)
		for j, t := range tags {
			c.Set(pfx+".Tag."+strconv.Itoa(j+1)+".Key", t[0])
			c.Set(pfx+".Tag."+strconv.Itoa(j+1)+".Value", t[1])
		}
	}
	b, err := a.api.ec2(ctx, "CreateImage", c)
	if err != nil {
		return "", fmt.Errorf("CreateImage: %w", err)
	}
	ami = xmlFirst(b, "imageId")
	log("creating image %s (%s); this takes a few minutes", ami, name)
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ami, ctx.Err()
		case <-time.After(10 * time.Second):
		}
		q := url.Values{}
		q.Set("ImageId.1", ami)
		b, err := a.api.ec2(ctx, "DescribeImages", q)
		if err != nil {
			continue
		}
		switch xmlFirst(b, "imagesSet>item>imageState") {
		case "available":
			a.pruneBakes(ctx, 2, log)
			return ami, nil
		case "failed":
			return ami, fmt.Errorf("image %s failed", ami)
		}
	}
	return ami, fmt.Errorf("image %s not available after 30m", ami)
}

// imageByName finds one of our images by its unique name (any state).
func (a *AWSSpot) imageByName(ctx context.Context, name string) string {
	p := url.Values{}
	p.Set("Owner.1", "self")
	p.Set("Filter.1.Name", "name")
	p.Set("Filter.1.Value.1", name)
	b, err := a.api.ec2(ctx, "DescribeImages", p)
	if err != nil {
		return ""
	}
	return xmlFirst(b, "imagesSet>item>imageId")
}

// deregister removes an image and its snapshots, reporting snapshots AWS
// could not delete (DeregisterImage succeeds even then).
func (a *AWSSpot) deregister(ctx context.Context, ami string, log Logger) error {
	p := url.Values{}
	p.Set("ImageId", ami)
	p.Set("DeleteAssociatedSnapshots", "true")
	b, err := a.api.ec2(ctx, "DeregisterImage", p)
	if err != nil {
		return err
	}
	var v struct {
		Results []struct {
			Snapshot string `xml:"snapshotId"`
			Code     string `xml:"returnCode"`
		} `xml:"deleteSnapshotResultSet>item"`
	}
	_ = xml.Unmarshal(b, &v)
	var bad []string
	for _, r := range v.Results {
		if r.Code != "" && r.Code != "success" && r.Code != "skipped" {
			bad = append(bad, r.Snapshot+" ("+r.Code+")")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("image %s removed but snapshots were not deleted: %s (needs ec2:DeleteSnapshot)", ami, strings.Join(bad, ", "))
	}
	if log != nil {
		log("removed image %s and its snapshots", ami)
	}
	return nil
}

func (a *AWSSpot) waitInstanceState(ctx context.Context, id, want string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		p := url.Values{}
		p.Set("InstanceId.1", id)
		if b, err := a.api.ec2(ctx, "DescribeInstances", p); err == nil {
			if got, _, err := parseDescribeInstances(b); err == nil && len(got) > 0 && got[0].State == want {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("instance %s did not reach %s", id, want)
}

// terminateAny terminates a builder (tagged spotexec-bake-runner + owner).
func (a *AWSSpot) terminateAny(ctx context.Context, id string) error {
	p := url.Values{}
	p.Set("InstanceId.1", id)
	var b []byte
	var err error
	// Just after launch the ID may not be visible yet: retry briefly
	// (1+2+4+8s), never sleeping after the last attempt.
	for attempt, wait := 0, time.Second; attempt < 5; attempt, wait = attempt+1, wait*2 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait / 2):
			}
		}
		if b, err = a.api.ec2(ctx, "DescribeInstances", p); err == nil || awsCode(err) != "InvalidInstanceID.NotFound" {
			break
		}
	}
	if err != nil {
		return err
	}
	got, _, err := parseDescribeInstances(b)
	if err != nil || len(got) == 0 {
		return err
	}
	owner, oerr := a.ownerTag()
	if oerr != nil || owner == "" {
		return fmt.Errorf("refusing to terminate %s: can't determine this machine's owner tag: %v", id, oerr)
	}
	if got[0].tag(tagOwner) != owner || (got[0].tag(tagBake) != a.r.Name && got[0].tag(tagRunner) != a.r.Name) {
		return fmt.Errorf("refusing to terminate %s: not ours", id)
	}
	t := url.Values{}
	t.Set("InstanceId.1", id)
	_, err = a.api.ec2(ctx, "TerminateInstances", t)
	return err
}

// pruneBakes deregisters all but the newest keep bakes of this runner,
// deleting their snapshots.
func (a *AWSSpot) pruneBakes(ctx context.Context, keep int, log Logger) {
	imgs, err := a.listBakes(ctx, false)
	if err != nil {
		return
	}
	for i, img := range imgs {
		if i < keep {
			continue
		}
		if err := a.deregister(ctx, img.ID, log); err != nil && log != nil {
			log("%v", err)
		}
	}
}

// Bakes lists this runner's bakes for display.
func (a *AWSSpot) Bakes(ctx context.Context) ([]string, error) {
	imgs, err := a.listBakes(ctx, false)
	if err != nil {
		return nil, err
	}
	cur := a.BakeHash()
	var out []string
	for _, i := range imgs {
		mark := ""
		if strings.Contains(i.Name, "-"+cur+"-") {
			mark = " (current bootstrap)"
		}
		out = append(out, fmt.Sprintf("%s  %s  %s%s", i.ID, i.Created, i.Name, mark))
	}
	return out, nil
}

// BakeImageCleanup terminates a builder after a failed bake.
func (a *AWSSpot) BakeImageCleanup(ctx context.Context, builderID string) (bool, error) {
	return true, a.terminateAny(ctx, builderID)
}

// ClearBakes deregisters every bake of this runner.
func (a *AWSSpot) ClearBakes(ctx context.Context) (int, error) {
	imgs, err := a.listBakesIn(ctx, false, "available", "pending", "failed")
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []string
	for _, img := range imgs {
		if err := a.deregister(ctx, img.ID, nil); err != nil {
			errs = append(errs, err.Error())
			if !strings.Contains(err.Error(), "snapshots were not deleted") {
				continue
			}
		}
		n++
	}
	if len(errs) > 0 {
		return n, errors.New(strings.Join(errs, "; "))
	}
	return n, nil
}

// LockBake takes this runner's bake lock (non-blocking) for the duration of
// a bake. A builder whose bake still holds the lock is never swept.
func (a *AWSSpot) LockBake() (unlock func(), err error) {
	dir := filepath.Join(config.StateDir(), "bake")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, a.r.Name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("a bake for %s is already running on this machine", a.r.Name)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// SweepBuilders terminates leftover bake builders of this runner (stopped,
// or older than four hours) unless a bake on this machine is in progress: a
// CLI that died mid-bake must not leave one billing. Called by `spotexec
// down`, the pool manager, and each bake (which holds the lock: see
// sweepBuilders).
func (a *AWSSpot) SweepBuilders(ctx context.Context, log Logger) {
	unlock, err := a.LockBake()
	if err != nil {
		return // a bake is running; its builder is not leftover
	}
	defer unlock()
	a.sweepBuilders(ctx, log)
}

// SweepBuildersLocked is SweepBuilders for a caller already holding the
// bake lock.
func (a *AWSSpot) SweepBuildersLocked(ctx context.Context, log Logger) { a.sweepBuilders(ctx, log) }

func (a *AWSSpot) sweepBuilders(ctx context.Context, log Logger) {
	owner, err := a.ownerTag()
	if err != nil {
		return
	}
	p := url.Values{}
	p.Set("Filter.1.Name", "tag:"+tagBake)
	p.Set("Filter.1.Value.1", a.r.Name)
	p.Set("Filter.2.Name", "tag:"+tagOwner)
	p.Set("Filter.2.Value.1", owner)
	p.Set("Filter.3.Name", "instance-state-name")
	for i, st := range []string{"pending", "running", "stopping", "stopped"} {
		p.Set("Filter.3.Value."+strconv.Itoa(i+1), st)
	}
	b, err := a.api.ec2(ctx, "DescribeInstances", p)
	if err != nil {
		return
	}
	insts, _, err := parseDescribeInstances(b)
	if err != nil {
		return
	}
	for _, i := range insts {
		// Four hours is past any bake's own budget (boot, bootstrap with
		// pre-pulls, and image creation).
		if i.State == "stopped" || time.Since(i.launched()) > 4*time.Hour {
			t := url.Values{}
			t.Set("InstanceId.1", i.InstanceID)
			if _, err := a.api.ec2(ctx, "TerminateInstances", t); err == nil && log != nil {
				log("terminated leftover bake builder %s", i.InstanceID)
			}
		}
	}
}

// BakeBootstrapTimeout is the per-script budget on a bake builder: the
// runner's boot_timeout plus room for the Docker install and pre-pulls.
func (a *AWSSpot) BakeBootstrapTimeout(prepull bool) time.Duration {
	t := a.r.BootTimeout.Duration + 10*time.Minute
	if prepull {
		t += 15 * time.Minute
	}
	return t
}

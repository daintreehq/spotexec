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
	"sync"
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

const (
	tagBakeHash = "spotexec-bootstrap-hash"
	tagBakeBase = "spotexec-base-ami" // the Ubuntu image a bake was built from
)

// A bake carries the OS packages of the day it was made, and VMs booted
// from it don't update them, so launches stop using a bake (and boot the
// current Ubuntu image with the full bootstrap instead) once it is
// BakeMaxAge old, or once Canonical has published a newer base image than
// the bake's and that has been out for BakeBaseGrace. Until then a bake on
// an older base is used but reported as stale.
const (
	BakeMaxAge    = 30 * 24 * time.Hour
	BakeBaseGrace = 7 * 24 * time.Hour
)

// BakeHash identifies what a bake contains; changing the bootstrap scripts,
// the login user or the architecture invalidates old bakes. The base AMI ID
// is not part of it (Canonical refreshes it every few weeks): it is tagged
// on the bake and judged separately (see BakeMaxAge).
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
	// VMs booted from the bake never run apt, so it carries the security
	// updates published since Canonical built the base image.
	b.WriteString("\n# spotexec bake: security updates since the base image\n")
	b.WriteString("apt-get update --error-on=any -o DPkg::Lock::Timeout=300 -qq\n")
	b.WriteString("command -v unattended-upgrade >/dev/null 2>&1 || apt-get install -y -qq -o DPkg::Lock::Timeout=300 unattended-upgrades\n")
	b.WriteString("unattended-upgrade\n")
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
	ID         string   `xml:"imageId"`
	Name       string   `xml:"name"`
	State      string   `xml:"imageState"`
	Created    string   `xml:"creationDate"`
	Arch       string   `xml:"architecture"`
	RootDevice string   `xml:"rootDeviceName"`
	Tags       []ec2Tag `xml:"tagSet>item"`
	Devices    []struct {
		Name string `xml:"deviceName"`
		Size int    `xml:"ebs>volumeSize"`
	} `xml:"blockDeviceMapping>item"`
}

func (i ec2Image) tag(k string) string {
	for _, t := range i.Tags {
		if t.Key == k {
			return t.Value
		}
	}
	return ""
}

func (i ec2Image) created() time.Time {
	t, _ := time.Parse(time.RFC3339, i.Created)
	return t
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
	if matchHash {
		// Picking an image to boot: any machine's bake of the same bootstrap
		// will do (bakes hold no keys or machine state), so one bake serves
		// every machine sharing the config.
		p.Set("Filter.4.Name", "tag:"+tagBakeHash)
		p.Set("Filter.4.Value.1", a.BakeHash())
	} else {
		// Listing, pruning and clearing: only this machine's own bakes.
		p.Set("Filter.4.Name", "tag:"+tagOwner)
		p.Set("Filter.4.Value.1", owner)
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

// Bake describes one baked image of a runner.
type Bake struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Arch    string    `json:"arch"`
	Hash    string    `json:"bootstrap_hash"`
	Base    string    `json:"base_ami,omitempty"` // "" = not recorded (made by an older spotexec)
	// Current: built from this runner's bootstrap, user and arch.
	Current bool `json:"current"`
	// InUse: the bake new VMs of this runner boot from.
	InUse bool `json:"in_use"`
	// NewerBase: unbaked VMs now boot a different (newer) Ubuntu base image,
	// first seen superseding this bake's at NewerSince.
	NewerBase  string    `json:"newer_base_ami,omitempty"`
	NewerSince time.Time `json:"newer_base_since,omitempty"`
	// Expired: launches no longer use it (see BakeMaxAge).
	Expired bool `json:"expired"`
	// Stale says why it is out of date, for people ("" = it is fresh).
	Stale string `json:"stale,omitempty"`
	// CheckErr: the current base image couldn't be looked up, so only the
	// bake's age was judged.
	CheckErr string `json:"check_error,omitempty"`

	img ec2Image
}

// Age is how long ago the bake was made.
func (b Bake) Age(now time.Time) time.Duration { return now.Sub(b.Created) }

func (a *AWSSpot) bakeOf(img ec2Image) Bake {
	b := Bake{ID: img.ID, Name: img.Name, Created: img.created(), Arch: img.Arch, Hash: img.tag(tagBakeHash), Base: img.tag(tagBakeBase), img: img}
	b.Current = b.Hash == a.BakeHash() && (b.Arch == "" || b.Arch == a.ec2Arch())
	return b
}

// judgeBake decides whether b is stale or expired. cur is the base image
// unbaked VMs boot now ("" = unknown: only the bake's age counts), and
// since is when b was first seen superseded by a newer one.
func judgeBake(b *Bake, cur string, since, now time.Time) {
	if cur != "" && cur != b.Base {
		b.NewerBase, b.NewerSince = cur, since
	}
	days := func(d time.Duration) int { return int(d.Hours() / 24) }
	why := fmt.Sprintf("a newer Ubuntu base image (%s) is out", b.NewerBase)
	if b.Base == "" {
		why = "its base image wasn't recorded (made by an older spotexec)"
	}
	until := b.Created.Add(BakeMaxAge)
	if b.NewerBase != "" && b.NewerSince.Add(BakeBaseGrace).Before(until) {
		until = b.NewerSince.Add(BakeBaseGrace)
	}
	switch age := b.Age(now); {
	case age > BakeMaxAge:
		b.Expired = true
		b.Stale = fmt.Sprintf("%d days old (bakes are used for %d days)", days(age), days(BakeMaxAge))
	case b.NewerBase != "" && now.After(until):
		b.Expired = true
		b.Stale = fmt.Sprintf("%s, and a stale bake is used for %d days", why, days(BakeBaseGrace))
	case b.NewerBase != "":
		b.Stale = fmt.Sprintf("%s; used until %s", why, until.Local().Format("Jan 2"))
	}
}

// supersededSince is when bake id became superseded: when the newer base
// image seen now was published (published; zero = unknown, then now). It
// is never before the bake itself (its base was current then). The first
// answer is kept in the state dir, so a later Ubuntu release can't restart
// the grace period.
func supersededSince(id string, bakeCreated, published, now time.Time) time.Time {
	since := now
	if !published.IsZero() && published.Before(now) {
		since = published
	}
	if since.Before(bakeCreated) {
		since = bakeCreated
	}
	dir := filepath.Join(config.StateDir(), "bake")
	p := filepath.Join(dir, "superseded-"+id)
	read := func() (time.Time, bool) {
		b, err := os.ReadFile(p)
		if err != nil {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		return t, err == nil
	}
	if t, ok := read(); ok {
		return t
	}
	// Written whole to a temp file, then linked into place, so a reader
	// never sees a partial file and the first writer wins; a damaged file
	// is replaced.
	if os.MkdirAll(dir, 0o700) != nil {
		return since
	}
	tmp, err := os.CreateTemp(dir, ".superseded-*")
	if err != nil {
		return since
	}
	_, werr := tmp.WriteString(since.UTC().Format(time.RFC3339) + "\n")
	cerr := tmp.Close()
	defer os.Remove(tmp.Name())
	if werr != nil || cerr != nil {
		return since
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		if t, ok := read(); ok {
			return t // another process won
		}
		_ = os.Rename(tmp.Name(), p) // damaged: replace it
	}
	return since
}

// baseImage is the Ubuntu image an unbaked VM of this runner boots now,
// and when Canonical published it (zero if unknown).
func (a *AWSSpot) baseImage(ctx context.Context) (string, time.Time, error) {
	ami, err := a.api.ssmGetParameter(ctx, a.amiParam())
	if err != nil {
		return "", time.Time{}, err
	}
	p := url.Values{}
	p.Set("ImageId.1", ami)
	var created time.Time
	if b, err := a.api.ec2(ctx, "DescribeImages", p); err == nil {
		created, _ = time.Parse(time.RFC3339, xmlFirst(b, "imagesSet>item>creationDate"))
	}
	return ami, created, nil
}

// baseRef is the current base image, looked up once per listing.
type baseRef struct {
	ami     string
	created time.Time
	err     error
}

func (a *AWSSpot) currentBase(ctx context.Context) baseRef {
	ami, created, err := a.baseImage(ctx)
	return baseRef{ami, created, err}
}

// judge applies judgeBake with the current base (bakes of another arch
// are judged by age alone: their base is a different image).
func (a *AWSSpot) judge(b *Bake, cur baseRef, now time.Time) {
	if b.Arch != "" && b.Arch != a.ec2Arch() {
		judgeBake(b, "", time.Time{}, now)
		return
	}
	if cur.err != nil {
		b.CheckErr = "couldn't look up the current Ubuntu base image: " + firstLineOf([]byte(cur.err.Error()))
	}
	var since time.Time
	if cur.ami != "" && cur.ami != b.Base {
		published := cur.created
		if b.Base == "" {
			published = time.Time{} // which base it predates is unknown: from now
		}
		since = supersededSince(b.ID, b.Created, published, now)
	}
	judgeBake(b, cur.ami, since, now)
}

// findBake returns the bake a launch would boot from: the newest matching
// the current bootstrap, judged against the current base image. It is nil
// when there is none; an expired one is returned (so callers can say why)
// but must not be used.
func (a *AWSSpot) findBake(ctx context.Context) (*Bake, error) {
	imgs, err := a.listBakes(ctx, true)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	b := a.bakeOf(imgs[0])
	a.judge(&b, a.currentBase(ctx), time.Now())
	b.InUse = !b.Expired
	return &b, nil
}

// CurrentBake is the bake new VMs of this runner would boot from (see
// findBake), or nil if there is none or the runner pins an AMI.
func (a *AWSSpot) CurrentBake(ctx context.Context) (*Bake, error) {
	if a.r.AMI != "" && a.r.AMI != "auto" {
		return nil, nil
	}
	return a.findBake(ctx)
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
	if inst.ImageID != "" {
		builderBases.Store(inst.InstanceID, inst.ImageID)
	}
	return a.instance(inst), nil
}

// builderBases remembers the image each builder this process launched
// booted from (builder ID -> AMI), for its bake's base tag.
var builderBases sync.Map

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
	// The base image the builder booted, so staleness can be judged later.
	base := a.builderBase(ctx, builderID)
	c := url.Values{}
	c.Set("InstanceId", builderID)
	c.Set("Name", name)
	c.Set("Description", "spotexec baked runner image for "+a.r.Name)
	tags := [][2]string{{tagRunner, a.r.Name}, {tagOwner, owner}, {tagBakeHash, a.BakeHash()}, {"Name", name}}
	if base != "" {
		tags = append(tags, [2]string{tagBakeBase, base})
	}
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

// builderBase is the image a builder booted from ("" if unknown).
func (a *AWSSpot) builderBase(ctx context.Context, builderID string) string {
	if v, ok := builderBases.Load(builderID); ok {
		return v.(string)
	}
	q := url.Values{}
	q.Set("InstanceId.1", builderID)
	b, err := a.api.ec2(ctx, "DescribeInstances", q)
	if err != nil {
		return ""
	}
	got, _, err := parseDescribeInstances(b)
	if err != nil || len(got) == 0 {
		return ""
	}
	return got[0].ImageID
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

// Bakes lists this runner's bakes (newest first), each judged against the
// current base image, with the one new VMs boot from marked InUse.
func (a *AWSSpot) Bakes(ctx context.Context) ([]Bake, error) {
	imgs, err := a.listBakes(ctx, false)
	if err != nil {
		return nil, err
	}
	// The bake launches pick, chosen exactly as they do (it may have been
	// made by another machine).
	inUse := ""
	if b, err := a.CurrentBake(ctx); err == nil && b != nil && b.InUse {
		inUse = b.ID
	}
	cur, now := a.currentBase(ctx), time.Now()
	var out []Bake
	for _, img := range imgs {
		b := a.bakeOf(img)
		a.judge(&b, cur, now)
		b.InUse = b.ID == inUse
		out = append(out, b)
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

// bakeCheck is doctor's view of the runner's bake: which one new VMs boot
// from, its base image and age, and whether it is stale.
func (a *AWSSpot) bakeCheck(ctx context.Context) Check {
	c := Check{Name: "baked image", OK: true}
	if a.r.AMI != "" && a.r.AMI != "auto" {
		c.Msg = fmt.Sprintf("not used (ami = %q)", a.r.AMI)
		return c
	}
	b, err := a.findBake(ctx)
	switch {
	case err != nil:
		c.OK, c.Msg = false, "listing bakes: "+err.Error()
	case b == nil:
		c.Msg = fmt.Sprintf("none: new VMs run the bootstrap (`spotexec image bake %s` makes cold starts faster)", a.r.Name)
	default:
		c.Msg = b.Summary(time.Now())
		if b.Expired {
			c.Msg += fmt.Sprintf("; not used: %s, so new VMs boot the current Ubuntu image and run the bootstrap. Run `spotexec image bake %s`", b.Stale, a.r.Name)
		} else if b.Stale != "" {
			c.Msg += fmt.Sprintf("; stale: %s. Run `spotexec image bake %s`", b.Stale, a.r.Name)
		}
		if b.CheckErr != "" {
			c.Msg += "; " + b.CheckErr
		}
	}
	return c
}

// Summary is "ami-…, N days old, base ami-…" for people.
func (b Bake) Summary(now time.Time) string {
	base := b.Base
	if base == "" {
		base = "not recorded"
	}
	return fmt.Sprintf("%s, %s old, base %s", b.ID, AgeText(b.Age(now)), base)
}

// AgeText is a short age: 40m, 30h, 12d.
func AgeText(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

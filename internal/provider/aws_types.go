package provider

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// typesTTL is how long a region's eligible instance types are reused: AWS
// adds types rarely, and prices (not this list) decide each launch.
const typesTTL = 24 * time.Hour

// InstanceTypeInfo is what spotexec needs to know about one EC2 instance
// type to decide whether a vcpus-based runner may use it.
type InstanceTypeInfo struct {
	Type      string `json:"type"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	Arch      string `json:"arch"` // amd64 | arm64
	// Why it's left out ("" when eligible); describe-time facts only.
	Excluded string `json:"-"`
}

// MemoryGiB is the type's memory in GiB.
func (t InstanceTypeInfo) MemoryGiB() float64 { return float64(t.MemoryMiB) / 1024 }

type describedType struct {
	Type       string   `xml:"instanceType"`
	Current    bool     `xml:"currentGeneration"`
	BareMetal  bool     `xml:"bareMetal"`
	Burstable  bool     `xml:"burstablePerformanceSupported"`
	UsageClass []string `xml:"supportedUsageClasses>item"`
	Archs      []string `xml:"processorInfo>supportedArchitectures>item"`
	VCPUs      int      `xml:"vCpuInfo>defaultVCpus"`
	MemoryMiB  int      `xml:"memoryInfo>sizeInMiB"`
	// Present (non-empty) only on accelerated types.
	GPU       innerXML `xml:"gpuInfo"`
	FPGA      innerXML `xml:"fpgaInfo"`
	Inference innerXML `xml:"inferenceAcceleratorInfo"`
	Neuron    innerXML `xml:"neuronInfo"`
	Media     innerXML `xml:"mediaAcceleratorInfo"`
}

type innerXML struct {
	Inner string `xml:",innerxml"`
}

func (x innerXML) set() bool { return strings.TrimSpace(x.Inner) != "" }

// parseDescribeInstanceTypes reads one DescribeInstanceTypes page into
// infos, each marked with why it is excluded for runner r ("" if usable).
func parseDescribeInstanceTypes(b []byte, r *config.Runner) ([]InstanceTypeInfo, string, error) {
	var v struct {
		Items []describedType `xml:"instanceTypeSet>item"`
		Next  string          `xml:"nextToken"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, "", fmt.Errorf("parse DescribeInstanceTypes: %w", err)
	}
	out := make([]InstanceTypeInfo, 0, len(v.Items))
	for _, d := range v.Items {
		if d.Type == "" {
			continue
		}
		info := InstanceTypeInfo{Type: d.Type, VCPUs: d.VCPUs, MemoryMiB: d.MemoryMiB}
		for _, a := range d.Archs {
			switch a {
			case "x86_64":
				info.Arch = "amd64"
			case "arm64":
				info.Arch = "arm64"
			}
		}
		info.Excluded = excludedType(d, info, r)
		out = append(out, info)
	}
	return out, strings.TrimSpace(v.Next), nil
}

// excludedType says why a described type is no use to a vcpus-based
// runner ("" when it qualifies).
func excludedType(d describedType, info InstanceTypeInfo, r *config.Runner) string {
	switch {
	case info.VCPUs != r.VCPUs:
		return fmt.Sprintf("%d vCPUs", info.VCPUs)
	case info.Arch != r.Arch:
		return "not " + r.Arch
	case float64(info.MemoryMiB) < r.MinMemoryGiB*1024:
		return fmt.Sprintf("%.4g GiB", info.MemoryGiB())
	case !d.Current:
		return "previous generation"
	case d.BareMetal:
		return "bare metal"
	case d.Burstable:
		return "burstable"
	case !contains(d.UsageClass, "spot"):
		return "no Spot"
	case d.GPU.set() || d.FPGA.set() || d.Inference.set() || d.Neuron.set() || d.Media.set():
		return "accelerator"
	}
	return excludedName(info.Type, r)
}

// excludedName applies the name-based rules: the standard Spot quota's
// families only (one quota for the whole fleet), no Flex types (they don't
// sustain full CPU), generation, and the runner's patterns.
func excludedName(t string, r *config.Runner) string {
	fam, _, _ := strings.Cut(t, ".")
	switch {
	case !standardFamily(t) || strings.HasPrefix(fam, "mac"):
		return "outside the standard Spot quota"
	case strings.HasSuffix(fam, "-flex"):
		return "flex (not sustained full CPU)"
	case typeGeneration(t) < r.MinGeneration:
		return fmt.Sprintf("generation %d", typeGeneration(t))
	}
	for _, p := range r.ExcludeInstanceTypes {
		if config.MatchType(p, t) {
			return "excluded by " + p
		}
	}
	if len(r.InstanceTypes) > 0 && listedIndex(r, t) < 0 {
		return "not in instance_types"
	}
	return ""
}

// typeGeneration is the generation digit(s) of an instance family (m7a: 7,
// x2iedn: 2), 0 when there are none.
func typeGeneration(t string) int {
	fam, _, _ := strings.Cut(t, ".")
	i := strings.IndexAny(fam, "0123456789")
	if i < 0 {
		return 0
	}
	j := i
	for j < len(fam) && fam[j] >= '0' && fam[j] <= '9' {
		j++
	}
	n, _ := strconv.Atoi(fam[i:j])
	return n
}

// listedIndex is the position of the first instance_types entry (a name or
// pattern) matching t, or -1.
func listedIndex(r *config.Runner, t string) int {
	for i, p := range r.InstanceTypes {
		if config.MatchType(p, t) {
			return i
		}
	}
	return -1
}

// typesKey identifies what a cached type list was selected for.
func typesKey(r *config.Runner) string {
	return fmt.Sprintf("%s|%s|%d|%g|%d|%s|%s", r.Profile, r.Arch, r.VCPUs, r.MinMemoryGiB, r.MinGeneration,
		strings.Join(r.InstanceTypes, ","), strings.Join(r.ExcludeInstanceTypes, ","))
}

type typesCache struct {
	Key   string             `json:"key"`
	At    time.Time          `json:"at"`
	Types []InstanceTypeInfo `json:"types"`
}

func (a *AWSSpot) typesCachePath() string {
	return filepath.Join(a.dir, "instance-types-"+a.r.Region+".json")
}

// EligibleTypes lists the instance types a vcpus-based runner may launch
// in its region, smallest memory first, from a day-long cache unless
// refresh is set. Not for runners with a fixed instance_types list.
func (a *AWSSpot) EligibleTypes(ctx context.Context, refresh bool) ([]InstanceTypeInfo, error) {
	if !a.r.ByRequirements() {
		return nil, errors.New("not a vcpus-based runner")
	}
	key := typesKey(a.r)
	now := time.Now()
	if !refresh {
		if b, err := os.ReadFile(a.typesCachePath()); err == nil {
			var c typesCache
			if json.Unmarshal(b, &c) == nil && c.Key == key && now.Sub(c.At) < typesTTL && now.After(c.At) {
				if len(c.Types) == 0 {
					return nil, &noEligibleError{a.r}
				}
				return c.Types, nil
			}
		}
	}
	all, err := a.describeTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list instance types in %s: %w%s", a.r.Region, err, typesPermHint(err))
	}
	var out []InstanceTypeInfo
	for _, t := range all {
		if t.Excluded == "" {
			out = append(out, t)
		}
	}
	sortTypes(out)
	// An empty answer is cached too: it is as definite as a full one.
	if err := os.MkdirAll(a.dir, 0o700); err == nil {
		b, _ := json.MarshalIndent(typesCache{Key: key, At: now, Types: out}, "", "  ")
		tmp := a.typesCachePath() + ".tmp" + strconv.Itoa(os.Getpid())
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, a.typesCachePath())
		}
	}
	if len(out) == 0 {
		return nil, &noEligibleError{a.r}
	}
	return out, nil
}

// noEligibleError is EligibleTypes' answer when the region was described
// and none of its types qualifies (as opposed to a failed lookup).
type noEligibleError struct{ r *config.Runner }

func (e *noEligibleError) Error() string {
	return fmt.Sprintf("no instance type in %s has %s; lower min_memory_gib or min_generation, or check exclude_instance_types and instance_types", e.r.Region, describeRequirements(e.r))
}

func sortTypes(ts []InstanceTypeInfo) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].MemoryMiB != ts[j].MemoryMiB {
			return ts[i].MemoryMiB < ts[j].MemoryMiB
		}
		return ts[i].Type < ts[j].Type
	})
}

// describeTypes asks EC2 for the region's current-generation types with
// the runner's vCPU count and architecture, each marked with why it is
// excluded. The filters keep it to a page or two.
func (a *AWSSpot) describeTypes(ctx context.Context) ([]InstanceTypeInfo, error) {
	arch := "x86_64"
	if a.r.Arch == "arm64" {
		arch = "arm64"
	}
	var out []InstanceTypeInfo
	seen := map[string]bool{}
	token := ""
	for page := 0; ; page++ {
		p := url.Values{}
		p.Set("Filter.1.Name", "vcpu-info.default-vcpus")
		p.Set("Filter.1.Value.1", strconv.Itoa(a.r.VCPUs))
		p.Set("Filter.2.Name", "processor-info.supported-architecture")
		p.Set("Filter.2.Value.1", arch)
		p.Set("Filter.3.Name", "current-generation")
		p.Set("Filter.3.Value.1", "true")
		p.Set("MaxResults", "100")
		if token != "" {
			p.Set("NextToken", token)
		}
		b, err := a.api.ec2(ctx, "DescribeInstanceTypes", p)
		if err != nil {
			return nil, err
		}
		got, next, err := parseDescribeInstanceTypes(b, a.r)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
		if next == "" {
			return out, nil
		}
		if seen[next] || page > 50 {
			return nil, errors.New("DescribeInstanceTypes pagination did not end")
		}
		seen[next] = true
		token = next
	}
}

func typesPermHint(err error) string {
	if awsCode(err) == "UnauthorizedOperation" || strings.Contains(err.Error(), "UnauthorizedOperation") {
		return " (vcpus-based runners need ec2:DescribeInstanceTypes: re-run `spotexec setup` to update the policy)"
	}
	return ""
}

// describeRequirements says what a vcpus-based runner asks for.
func describeRequirements(r *config.Runner) string {
	s := fmt.Sprintf("%d vCPUs", r.VCPUs)
	if r.MinMemoryGiB > 0 {
		s += fmt.Sprintf(", at least %g GiB", r.MinMemoryGiB)
	}
	s += fmt.Sprintf(", generation %d or newer, %s", r.MinGeneration, r.Arch)
	return s
}

// DescribeRequirements is describeRequirements for commands.
func DescribeRequirements(r *config.Runner) string { return describeRequirements(r) }

// VMVCPUs is the vCPUs of one of the runner's VMs: vcpus, else the first
// listed instance type's.
func VMVCPUs(r *config.Runner) int {
	if r.VCPUs > 0 {
		return r.VCPUs
	}
	for _, t := range r.InstanceTypes {
		if !config.IsTypePattern(t) {
			return vcpusOf(t)
		}
	}
	return 0
}

// MinLaunchVCPUsFor is the smallest Spot launch the runner can make under
// the standard Spot quota (0 when none of its types uses it). vcpus-based
// runners only use standard-quota families.
func MinLaunchVCPUsFor(r *config.Runner) int {
	if r.ByRequirements() {
		return r.VCPUs
	}
	return minStandardVCPUs(r.InstanceTypes)
}

// HasOtherFamiliesFor reports whether the runner can launch types outside
// the standard Spot quota.
func HasOtherFamiliesFor(r *config.Runner) bool {
	if r.ByRequirements() {
		return false
	}
	return HasOtherFamilies(r.InstanceTypes)
}

// launchTypes is the types a launch in a's region may use: the listed
// ones, or (vcpus) every eligible type.
func (a *AWSSpot) launchTypes(ctx context.Context) ([]string, error) {
	if !a.r.ByRequirements() {
		return a.r.InstanceTypes, nil
	}
	ts, err := a.EligibleTypes(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Type
	}
	return out, nil
}

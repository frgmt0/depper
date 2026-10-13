package host

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/frgmt0/depper/internal/shell"
)

// Record is /etc/depper/host.json: what depper set up on the host. It never
// contains secrets.
type Record struct {
	DepperVersion string    `json:"depper_version"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
	DeployUser    string    `json:"deploy_user"`
	Container     string    `json:"container"`
	Cloudflared   bool      `json:"cloudflared"`
	Steps         []string  `json:"steps"`
	Packages      []string  `json:"packages"`
	Hardening     Hardening `json:"hardening"`
}

// Hardening lists the opt-in hardening depper applied.
type Hardening struct {
	SSH         bool  `json:"ssh"`
	Firewall    []int `json:"firewall,omitempty"`
	AutoUpdates bool  `json:"auto_updates"`
	Tailscale   bool  `json:"tailscale"`
}

// mergeRecord combines the previous record with this run's settings.
// Timestamps and version are left for the caller.
func mergeRecord(prev *Record, o *Options, steps, packages []string) Record {
	rec := Record{DeployUser: o.User, Container: o.Container, Cloudflared: o.Cloudflared}
	rec.Hardening = Hardening{SSH: o.HardenSSH, AutoUpdates: o.AutoUpdates, Tailscale: o.TailscaleKey.Set()}
	if o.Firewall != nil {
		rec.Hardening.Firewall = firewallPorts(o)
	}
	if prev != nil {
		rec.CreatedAt = prev.CreatedAt
		if prev.DeployUser == o.User {
			if rec.Container == ContainerNone && prev.Container != "" {
				rec.Container = prev.Container
			}
			rec.Cloudflared = rec.Cloudflared || prev.Cloudflared
			rec.Hardening.SSH = rec.Hardening.SSH || prev.Hardening.SSH
			rec.Hardening.AutoUpdates = rec.Hardening.AutoUpdates || prev.Hardening.AutoUpdates
			rec.Hardening.Tailscale = rec.Hardening.Tailscale || prev.Hardening.Tailscale
			rec.Hardening.Firewall = unionInts(prev.Hardening.Firewall, rec.Hardening.Firewall)
		}
		steps = append(append([]string{}, prev.Steps...), steps...)
		packages = append(append([]string{}, prev.Packages...), packages...)
	}
	rec.Steps = unionStrings(steps)
	rec.Packages = unionStrings(packages)
	return rec
}

// sameContent reports whether two records differ only in timestamps and
// version.
func sameContent(a, b *Record) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := *a, *b
	x.CreatedAt, x.UpdatedAt, x.DepperVersion = "", "", ""
	y.CreatedAt, y.UpdatedAt, y.DepperVersion = "", "", ""
	if len(x.Hardening.Firewall) == 0 {
		x.Hardening.Firewall = nil
	}
	if len(y.Hardening.Firewall) == 0 {
		y.Hardening.Firewall = nil
	}
	return reflect.DeepEqual(x, y)
}

func unionStrings(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func unionInts(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range append(append([]int{}, a...), b...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// recordLines writes rec to RecordPath as root, mode 644.
func (r *Runner) recordLines(rec Record) ([]string, error) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	dir := r.raw("/etc/depper")
	return []string{
		"umask 022",
		"mkdir -p " + shell.Quote(dir),
		"chmod 755 " + shell.Quote(dir),
		"printf '%s\\n' " + shell.Quote(string(data)) + " > " + shell.Quote(r.raw(RecordPath)+".depper-tmp"),
		"chown root:root " + shell.Quote(r.raw(RecordPath)+".depper-tmp"),
		"chmod 644 " + shell.Quote(r.raw(RecordPath)+".depper-tmp"),
		"mv -f " + shell.Quote(r.raw(RecordPath)+".depper-tmp") + " " + r.p(RecordPath),
	}, nil
}

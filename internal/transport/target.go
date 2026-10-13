package transport

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
)

// Target is how to reach the deploy host over SSH. Fields mirror the
// [deploy] keys; all are optional except Host.
type Target struct {
	// Host is an SSH alias, DNS name, IPv4, or bare IPv6 literal, optionally
	// prefixed with "user@".
	Host string
	// User is passed as -l (mutually exclusive with a user@ prefix).
	User string
	// Port is passed as -p when non-zero.
	Port int
	// IdentityFile is passed as -i with IdentitiesOnly=yes.
	IdentityFile string
	// KnownHosts sets UserKnownHostsFile and StrictHostKeyChecking=yes.
	KnownHosts string
	// Options are extra "-o" values (for example "ForwardAgent=no"), added
	// after the options above and before BatchMode=yes.
	Options []string
}

// SSHOptions returns the ssh options shared by every ssh and rsync
// invocation, ending with BatchMode=yes so depper never prompts.
func (t Target) SSHOptions() []string {
	var opts []string
	if t.Port != 0 {
		opts = append(opts, "-p", strconv.Itoa(t.Port))
	}
	if t.IdentityFile != "" {
		opts = append(opts, "-i", t.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	if t.User != "" {
		opts = append(opts, "-l", t.User)
	}
	if t.KnownHosts != "" {
		opts = append(opts, "-o", "UserKnownHostsFile="+t.KnownHosts, "-o", "StrictHostKeyChecking=yes")
	}
	for _, o := range t.Options {
		opts = append(opts, "-o", o)
	}
	return append(opts, "-o", "BatchMode=yes")
}

// SplitHost splits an optional "user@" prefix from Host.
func (t Target) SplitHost() (user, addr string) {
	if i := strings.LastIndexByte(t.Host, '@'); i >= 0 {
		return t.Host[:i], t.Host[i+1:]
	}
	return "", t.Host
}

// IsIPv6 reports whether the host part is an IPv6 literal (the only valid
// host spelling that contains a colon).
func (t Target) IsIPv6() bool {
	_, addr := t.SplitHost()
	return strings.Contains(addr, ":")
}

// RsyncHost returns the host spelled for an rsync remote destination:
// IPv6 literals are bracketed ("user@[2001:db8::1]").
func (t Target) RsyncHost() string {
	user, addr := t.SplitHost()
	if t.IsIPv6() {
		addr = "[" + addr + "]"
	}
	if user != "" {
		return user + "@" + addr
	}
	return addr
}

// SSHArgs returns argv for running remoteCommand on the host:
// ssh <options> -T -- <host> <remoteCommand>. -T: scripts never get a
// pseudo-terminal (no tty echo, no CR/LF translation, no prompts).
func (t Target) SSHArgs(program, remoteCommand string) []string {
	args := append([]string{program}, t.SSHOptions()...)
	args = append(args, "-T", "--", t.Host)
	if remoteCommand != "" {
		args = append(args, remoteCommand)
	}
	return args
}

// RsyncShell returns the value for rsync's -e option: the ssh program and
// the shared options, each word quoted for rsync's own -e splitter (which
// honours single and double quotes, with a doubled quote as a literal).
func (t Target) RsyncShell(program string) string {
	words := append([]string{program}, t.SSHOptions()...)
	for i, w := range words {
		words[i] = rsyncWord(w)
	}
	return strings.Join(words, " ")
}

func rsyncWord(w string) string {
	if w != "" && !strings.ContainsAny(w, " \t\n'\"") {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", "''") + "'"
}

// RsyncArgs returns argv that copies the contents of the local directory src
// into remoteDir on the host. remoteDir must pass shell.IsSafePath and is
// passed unquoted: rsync >= 3.2.4 protects its arguments from the remote
// shell (and would turn quotes into literal path characters), while older
// versions hand the path to the remote shell, where the safe alphabet needs
// no quoting either.
func (t Target) RsyncArgs(program, sshProgram, src, remoteDir string, excludes []string) ([]string, error) {
	if !shell.IsSafePath(remoteDir) {
		return nil, fmt.Errorf("refusing rsync to remote path %q: only absolute paths made of %s are supported", remoteDir, shell.SafePathChars)
	}
	if src == "" {
		return nil, fmt.Errorf("rsync source directory is empty")
	}
	args := []string{program, "-az", "-e", t.RsyncShell(sshProgram)}
	seen := map[string]bool{}
	for _, pattern := range excludes {
		if !seen[pattern] {
			seen[pattern] = true
			args = append(args, "--exclude", pattern)
		}
	}
	dir := strings.TrimRight(remoteDir, "/")
	return append(args, strings.TrimRight(src, "/")+"/", t.RsyncHost()+":"+dir+"/"), nil
}

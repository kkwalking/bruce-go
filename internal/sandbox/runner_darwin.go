//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

type seatbeltRunner struct{}

func newPlatformRunner(string) Runner { return seatbeltRunner{} }

func (seatbeltRunner) Probe(ctx context.Context) Capabilities {
	spec := CommandSpec{
		Directory:      "/",
		Environment:    []string{"PATH=/usr/bin:/bin"},
		Timeout:        5 * time.Second,
		MaxOutputChars: 2000,
	}
	profile := `(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow file-read*)`
	result, err := runProcess(ctx, seatbeltExecutable, []string{"-p", profile, "--", "/usr/bin/true"}, spec)
	if err != nil {
		return Capabilities{Backend: "seatbelt", Reason: err.Error()}
	}
	if result.ExitCode != 0 {
		return Capabilities{Backend: "seatbelt", Reason: strings.TrimSpace(result.Output)}
	}
	return Capabilities{Backend: "seatbelt", Available: true}
}

func (seatbeltRunner) Run(ctx context.Context, spec CommandSpec, policy Policy) (RunResult, error) {
	profile, definitions := buildSeatbeltProfile(policy)
	args := []string{"-p", profile}
	for _, definition := range definitions {
		args = append(args, "-D"+definition)
	}
	args = append(args, "--", "/bin/bash", "--noprofile", "--norc", "-c", spec.Command)
	result, err := runProcess(ctx, seatbeltExecutable, args, spec)
	if err != nil {
		return result, fmt.Errorf("Seatbelt 启动失败: %w", err)
	}
	return result, nil
}

func buildSeatbeltProfile(policy Policy) (string, []string) {
	var profile strings.Builder
	profile.WriteString(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow file-read*)
(allow file-write-data (literal "/dev/null"))
(allow sysctl-read)
(allow sysctl-write (sysctl-name "kern.grade_cputype"))
(allow ipc-posix-sem)
(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))
(allow file-read* file-write* (require-all (regex #"^/dev/ttys[0-9]+") (extension "com.apple.sandbox.pty")))
(allow file-ioctl (regex #"^/dev/ttys[0-9]+"))
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.logd")
  (global-name "com.apple.cfprefsd.agent")
  (global-name "com.apple.cfprefsd.daemon"))
`)

	definitions := []string{}
	protected := append([]string(nil), policy.Git.ProtectedPaths...)
	writeRoots := []string{policy.TempRoot}
	if policy.Mode == ModeWorkspaceWrite {
		writeRoots = append(writeRoots, policy.WorkspaceRoot)
		writeRoots = append(writeRoots, policy.Git.WriteRoots...)
	}
	for index, root := range uniqueCleanPaths(writeRoots) {
		key := fmt.Sprintf("WRITABLE_%d", index)
		definitions = append(definitions, key+"="+root)
		fmt.Fprintf(&profile, "(allow file-write* (require-all (subpath (param \"%s\"))", key)
		for protectedIndex, path := range uniqueCleanPaths(protected) {
			protectedKey := fmt.Sprintf("PROTECTED_%d", protectedIndex)
			if index == 0 {
				definitions = append(definitions, protectedKey+"="+path)
			}
			fmt.Fprintf(&profile, " (require-not (literal (param \"%s\"))) (require-not (subpath (param \"%s\")))", protectedKey, protectedKey)
		}
		profile.WriteString("))\n")
	}

	for index, path := range uniqueCleanPaths(append(policy.SensitivePaths, policy.SocketPaths...)) {
		key := fmt.Sprintf("SENSITIVE_%d", index)
		definitions = append(definitions, key+"="+path)
		fmt.Fprintf(&profile, "(deny file-read* (literal (param \"%s\")) (subpath (param \"%s\")))\n", key, key)
		fmt.Fprintf(&profile, "(deny file-write* (literal (param \"%s\")) (subpath (param \"%s\")))\n", key, key)
	}

	if policy.NetworkAccess {
		profile.WriteString(`(allow system-socket (require-all (socket-domain AF_SYSTEM) (socket-protocol 2)))
(allow network-bind (local ip "*:*") )
(allow network-inbound (local ip "*:*") )
(allow network-outbound (remote ip "*:*") )
(allow network-outbound (literal "/private/var/run/mDNSResponder"))
(allow mach-lookup
  (global-name "com.apple.networkd")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd"))
`)
	}
	return profile.String(), definitions
}

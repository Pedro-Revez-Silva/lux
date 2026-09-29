package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/spec"
)

// Nested containers: a Run with sandbox.nestedContainers can run rootless
// Podman or Docker inside its container. It gets, beyond every other Run's
// containment, only what that needs, and never --privileged:
//
//   - CAP_SYS_CHROOT (Podman's storage setup chroots);
//   - no no-new-privileges: newuidmap and newgidmap gain CAP_SETUID and
//     CAP_SETGID from their file capabilities. The workload starts with no
//     capabilities, but a setuid-root program in its image can make it
//     root in its container: as for a Run whose workload is root, that is
//     within the Run (its user namespace), never beyond the bounding set
//     every Run has;
//   - /dev/fuse (fuse-overlayfs) and /dev/net/tun (pasta, its network);
//   - unmask=ALL and label=disable (its /proc and /sys mounts);
//   - the host's seccomp profile, plus sethostname, setdomainname and setns:
//     the default profile allows those only with CAP_SYS_ADMIN, which
//     nested containers do not get; inside the Run's user namespace they
//     affect only namespaces the Run itself created;
//   - on a host whose AppArmor restricts unprivileged user namespaces
//     (Ubuntu 24.04 on), apparmor=unconfined: there a user namespace made
//     under a profile without a userns rule, as containers-default is,
//     loses its capabilities, and rootless Docker cannot even make its
//     network namespace. That also lifts the profile's other rules;
//     seccomp, the capability set and the Run's own user namespace still
//     bound it.
//
// The container is still in its own user namespace (a unique unprivileged
// host uid range), with its own network and egress rules: nested
// containers share the Run's network namespace's routes, so they have the
// Run's egress and nothing more.
func (p *placement) extraArgs(sp spec.RunSpec) []string {
	if !sp.Sandbox.NestedContainers {
		return nil
	}
	args := []string{
		"--cap-add=SYS_CHROOT",
		"--device=/dev/fuse", "--device=/dev/net/tun",
		"--security-opt=unmask=ALL", "--security-opt=label=disable",
		"--security-opt=seccomp=" + p.r.nestedSeccomp,
	}
	if p.r.nestedAppArmor {
		args = append(args, "--security-opt=apparmor=unconfined")
	}
	return args
}

// appArmorSysctl is where the kernel says whether AppArmor restricts
// unprivileged user namespaces.
const appArmorSysctl = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

func appArmorRestrictsUserns(sysctl string) bool {
	b, err := os.ReadFile(sysctl)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// nestedHome is the home the workload runs with: the spec's HOME, else its
// user's; "" if neither is absolute (a passwd entry with no home).
func nestedHome(user passwd.User, env map[string]string) string {
	for _, h := range []string{env["HOME"], user.Home} {
		if path.IsAbs(h) {
			return path.Clean(h)
		}
	}
	return ""
}

// nestedStores are where container engines inside a nested Run keep their
// images and layers: rootless Docker and Podman under $XDG_DATA_HOME
// (default ~/.local/share), rootful ones under /var/lib. env is what the
// workload gets (workloadEnv: the image's, then the spec's). None if the
// workload has no absolute home to put them in. (An engine configured
// elsewhere, say a Docker data-root, is not seen: its store stays on the
// container's root.)
func nestedStores(user passwd.User, env map[string]string) []string {
	if user.UID == 0 {
		return []string{"/var/lib/docker", "/var/lib/containers"}
	}
	share := env["XDG_DATA_HOME"]
	if !path.IsAbs(share) {
		home := nestedHome(user, env)
		if home == "" {
			return nil
		}
		share = path.Join(home, ".local", "share")
	}
	share = path.Clean(share)
	return []string{path.Join(share, "docker"), path.Join(share, "containers")}
}

// nestedVolumes gives each engine store its own ephemeral volume. On the
// container's own overlay root an engine cannot use native overlay (Linux
// has no overlay on overlay) and falls back to fuse-overlayfs or vfs, at
// a fraction of the speed; on a volume it gets overlay2. Ephemeral, so
// images and layers are never snapshotted, even under a state volume (the
// deeper mount wins). Only a spec volume at, inside, or around a store
// strictly under the home (say ~/.local/share) leaves that store to the
// spec: on purpose, then. One at the home or above it (/home, /workspace
// holding XDG_DATA_HOME) is not about the store.
func nestedVolumes(runID string, sp spec.RunSpec, user passwd.User, env map[string]string) []volumeRef {
	if !sp.Sandbox.NestedContainers {
		return nil
	}
	home := nestedHome(user, env)
	var refs []volumeRef
	for _, store := range nestedStores(user, env) {
		if slices.ContainsFunc(sp.Volumes, func(v spec.Volume) bool {
			p := path.Clean(v.Path)
			return spec.Under(p, store) || (spec.Under(store, p) && p != home && home != "" && spec.Under(p, home))
		}) {
			continue
		}
		base := path.Base(store)
		// "--" cannot collide with a spec volume's name (which cannot
		// start with "-").
		refs = append(refs, volumeRef{Name: "nested " + base, Volume: "lux-" + runID + "--" + base, Path: store, Kind: "ephemeral", Engine: true})
	}
	return refs
}

// nestedSyscalls are what the nested profile allows beyond the host's.
var nestedSyscalls = []string{"sethostname", "setdomainname", "setns"}

// writeNestedSeccomp derives the nested-containers seccomp profile from the
// host's default one and writes it under the data directory.
func writeNestedSeccomp(dataDir, hostProfile string) (string, error) {
	b, err := os.ReadFile(hostProfile)
	if err != nil {
		return "", fmt.Errorf("seccomp profile: %w", err)
	}
	var prof map[string]any
	if err := json.Unmarshal(b, &prof); err != nil {
		return "", fmt.Errorf("seccomp profile %s: %w", hostProfile, err)
	}
	rules, _ := prof["syscalls"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if rule["action"] != "SCMP_ACT_ERRNO" {
			continue
		}
		names, _ := rule["names"].([]any)
		kept := names[:0]
		for _, n := range names {
			if s, _ := n.(string); !slices.Contains(nestedSyscalls, s) {
				kept = append(kept, n)
			}
		}
		rule["names"] = kept
	}
	prof["syscalls"] = append(rules, map[string]any{
		"names": nestedSyscalls, "action": "SCMP_ACT_ALLOW", "args": []any{},
		"comment": "lux: nested containers",
	})
	out, err := json.Marshal(prof)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, "nested-seccomp.json")
	return path, writeFileAtomic(path, out, 0o644)
}

// hostSeccompProfile is the profile Podman uses by default on this host.
func (r *Runner) hostSeccompProfile(ctx context.Context) (string, error) {
	out, err := r.pm.Run(ctx, "info", "--format", "{{.Host.Security.SECCOMPProfilePath}}")
	if err != nil {
		return "", fmt.Errorf("podman's seccomp profile: %w", err)
	}
	if p := strings.TrimSpace(string(out)); p != "" {
		return p, nil
	}
	return "", errors.New("podman reports no default seccomp profile: nested containers need one to extend")
}

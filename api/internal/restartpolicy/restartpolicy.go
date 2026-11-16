// Package restartpolicy is the single source of truth for how a service-needing-restart is
// treated: whether it's the D-Bus bus (special-cased), whether restarting it is session-risky,
// whether it's disruptive enough to warn about, and — given the host's needrestart handlers and
// the learned "restarted-but-still-flagged" set — which units a smart restart can act on vs which
// genuinely need a reboot. Both the SSH client (which runs the restarts) and the scanner/db
// (which classifies a snapshot) import this so they can never drift apart.
package restartpolicy

import "strings"

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// IsDbusFamily reports whether the unit is the D-Bus bus itself. These are the ONLY units for
// which "no needrestart handler" must mean "reboot" rather than a detached restart — a naive
// restart of the bus severs it and strands its clients (logind, etc.) → SSH lockout.
func IsDbusFamily(svc string) bool {
	switch norm(svc) {
	case "dbus", "dbus.service", "dbus.socket", "dbus-broker", "dbus-broker.service":
		return true
	}
	return false
}

// IsKnownRebootOnly reports whether a unit is one we know, from practice, a live restart cannot
// clear: its code is coupled to the running kernel/runtime, so needrestart keeps flagging it until
// a reboot. containerd is the proven case — `needrestart -r a` (and a plain restart) leave it
// flagged on the same boot; only a reboot applies the change. Like the D-Bus bus, such a unit is
// treated as reboot-only UNLESS the host has a coordinated needrestart handler for it (see Classify)
// — that handler is the escape hatch for a host that genuinely can restart it in place.
//
// Keep this list SHORT and evidence-based. The general mechanism is to LEARN reboot-resistance per
// host (a unit we restarted that comes back still-flagged on the same boot); this static seed only
// spares the operator a known-futile first restart for units we've actually confirmed are coupled.
func IsKnownRebootOnly(svc string) bool {
	switch norm(svc) {
	case "containerd", "containerd.service":
		return true
	}
	return false
}

// noLiveExact / noLivePrefix: needrestart's own stock override_rc list (Debian 13 needrestart 3.11,
// Ubuntu 24.04 3.6), minus the daemons Patchdeck restarts on purpose as the opt-in alternative to a
// reboot (docker, network daemons, gettys, libvirt…). What's left are units where a restart is not
// a library refresh: it re-runs a one-time job (rc-local, cloud-init, cron-*, the google scripts),
// kills or starts an apt run (apt-daily*), ends sessions (user@N, display managers, xrdp), or breaks
// a shutdown helper (unattended-upgrades: restarted live on nova 2026-11-13 after a glibc update,
// the fresh copy exited 1 after 22 s and stayed failed for 3 days). Matching mirrors needrestart's
// regexes: exact names compare without ".service"; prefixes match the start of the name.
var noLiveExact = map[string]bool{
	"unattended-upgrades": true, "apt-daily": true, "apt-daily-upgrade": true,
	"rc-local": true, "emergency": true, "rescue": true,
	"cloud-init-local": true, "cloud-init": true, "cloud-config": true, "cloud-final": true,
	"google-shutdown-scripts": true, "google-startup-scripts": true,
}

var noLivePrefix = []string{
	"cron-",
	"gdm", "greetd", "kdm", "nodm", "sddm", "wdm", "xdm", "lightdm", "slim", "lxdm", "xrdp",
}

// IsNoLiveRestart reports whether a unit must never be restarted on a running host (see
// noLiveExact). Such a unit is left for the next reboot — and, unlike a reboot-only daemon, it does
// NOT make the host "need a reboot": running it on the old library until then is harmless, and
// counting it would flag nearly every Debian/Ubuntu host after each libc/python/openssl update.
func IsNoLiveRestart(svc string) bool {
	n := strings.TrimSuffix(norm(svc), ".service")
	if noLiveExact[n] {
		return true
	}
	if rest, ok := strings.CutPrefix(n, "user@"); ok && rest != "" && strings.Trim(rest, "0123456789") == "" {
		return true
	}
	for _, p := range noLivePrefix {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// SplitAtReboot splits a reboot-only list into the units that really need a reboot (loud) and the
// no-live-restart ones that simply clear at the next reboot (quiet; see IsNoLiveRestart).
func SplitAtReboot(rebootOnly []string) (loud, quiet []string) {
	for _, s := range rebootOnly {
		if IsNoLiveRestart(s) {
			quiet = append(quiet, s)
		} else {
			loud = append(loud, s)
		}
	}
	return
}

// CountLive counts the flagged units that matter now: everything except the no-live-restart ones.
func CountLive(needsRestart []string) int {
	n := 0
	for _, s := range needsRestart {
		if !IsNoLiveRestart(s) {
			n++
		}
	}
	return n
}

// IsRisky reports whether a plain `systemctl restart` over SSH is destructive to the session
// (the bus + the login-session manager). These must go through needrestart's coordinated
// handler or a detached restart — never a naive live restart.
func IsRisky(svc string) bool {
	if IsDbusFamily(svc) {
		return true
	}
	switch norm(svc) {
	case "systemd-logind", "systemd-logind.service":
		return true
	}
	return false
}

// IsDisruptive reports whether restarting the unit is likely to drop the operator's SSH session
// or bounce running workloads (containers, the network). Used only to warn in the confirm step —
// it does not change what a restart does, just what the user is told to expect.
func IsDisruptive(svc string) bool {
	if IsRisky(svc) {
		return true
	}
	switch norm(svc) {
	case "docker", "docker.service", "containerd", "containerd.service",
		"ssh", "ssh.service", "sshd", "sshd.service",
		"systemd-networkd", "systemd-networkd.service",
		"networkmanager", "networkmanager.service",
		"networking", "networking.service",
		"systemd-resolved", "systemd-resolved.service",
		"tailscaled", "tailscaled.service", "wg-quick@wg0.service":
		return true
	}
	return false
}

// Classify splits a host's needs-restart list into the units a smart "Restart services" can act
// on (restartable) versus the units that genuinely need a reboot (rebootOnly). A unit is
// reboot-only when it's learned-resistant (we restarted it and it came back on the same boot), or
// it's the D-Bus bus / a known kernel-coupled unit (e.g. containerd) with no coordinated handler on
// this host. Inputs:
//   - handlers[svc]:  the host has /etc/needrestart/restart.d/<svc> (a coordinated handler)
//   - resistant[svc]: it was restarted but is still flagged on the current boot
func Classify(needsRestart []string, handlers, resistant map[string]bool) (restartable, rebootOnly []string) {
	for _, s := range needsRestart {
		switch {
		case resistant[s]:
			rebootOnly = append(rebootOnly, s)
		case (IsDbusFamily(s) || IsKnownRebootOnly(s) || IsNoLiveRestart(s)) && !handlers[s]:
			rebootOnly = append(rebootOnly, s)
		default:
			restartable = append(restartable, s)
		}
	}
	return
}

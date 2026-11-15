package sshx

import "testing"

// TestKernelRebootFromNeedrestart: Polaris (Proxmox, 2026-11-15) ran 7.0.14-28-pve with -29
// installed. needrestart said KSTA 3, but nothing wrote /var/run/reboot-required (Debian/Proxmox
// don't), so Patchdeck said "no reboot". The scan now reads needrestart's verdict too.
func TestKernelRebootFromNeedrestart(t *testing.T) {
	polaris := "NEEDRESTART-VER: 3.11\nNEEDRESTART-KCUR: 7.0.14-28-pve\nNEEDRESTART-KEXP: 7.0.14-29-pve\nNEEDRESTART-KSTA: 3\nNEEDRESTART-UCSTA: 1\nNEEDRESTART-UCCUR: 0x00f0\nNEEDRESTART-UCEXP: 0x00f0\n"
	res := parseScanOutput(polaris, "h")
	if !res.NeedsReboot || res.RebootReason != "kernel 7.0.14-28-pve → 7.0.14-29-pve" {
		t.Errorf("Polaris: NeedsReboot=%v reason=%q", res.NeedsReboot, res.RebootReason)
	}
	for name, out := range map[string]string{
		"current kernel":   "NEEDRESTART-KCUR: 6.1.0-28\nNEEDRESTART-KEXP: 6.1.0-28\nNEEDRESTART-KSTA: 1\nNEEDRESTART-UCSTA: 1\n",
		"unknown (KSTA 0)": "NEEDRESTART-KSTA: 0\n",
		"no needrestart":   "__NEEDRESTART_MISSING__\n",
	} {
		if r := parseScanOutput(out, "h"); r.NeedsReboot {
			t.Errorf("%s: should not need a reboot (reason %q)", name, r.RebootReason)
		}
	}
	if r := parseScanOutput("NEEDRESTART-KSTA: 2\n", "h"); !r.NeedsReboot || r.RebootReason != "a newer kernel is installed" {
		t.Errorf("ABI-compatible kernel upgrade: %v %q", r.NeedsReboot, r.RebootReason)
	}
	if r := parseScanOutput("NEEDRESTART-KSTA: 1\nNEEDRESTART-UCSTA: 2\n", "h"); !r.NeedsReboot || r.RebootReason != "CPU microcode update" {
		t.Errorf("microcode: %v %q", r.NeedsReboot, r.RebootReason)
	}
	// The reboot-required file still counts, and its packages lead the reason.
	both := "__REBOOT__\n__REBOOT_PKGS_START__\nlinux-image-amd64\n__REBOOT_PKGS_END__\nNEEDRESTART-KCUR: a\nNEEDRESTART-KEXP: b\nNEEDRESTART-KSTA: 3\n"
	if r := parseScanOutput(both, "h"); !r.NeedsReboot || r.RebootReason != "linux-image-amd64; kernel a → b" {
		t.Errorf("both: %v %q", r.NeedsReboot, r.RebootReason)
	}
}

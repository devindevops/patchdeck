package sshx

import (
	"strings"
	"testing"

	"patchdeck/api/internal/models"
)

func TestParseScanOutput_Packages(t *testing.T) {
	raw := "Listing...\n" +
		"libfoo/jammy-security 1.2.3-1 amd64 [upgradable from: 1.2.2-1]\n" +
		"libbar/jammy-updates 2.0-1 amd64 [upgradable from: 1.9-1]\n"
	res := parseScanOutput(raw, "host-1")

	if res.HostID != "host-1" {
		t.Fatalf("HostID = %q, want host-1", res.HostID)
	}
	if len(res.Packages) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(res.Packages), res.Packages)
	}
	p := res.Packages[0]
	if p.Name != "libfoo" || p.Source != "jammy-security" || p.NewVersion != "1.2.3-1" || p.Arch != "amd64" || p.CurrentVersion != "1.2.2-1" {
		t.Fatalf("first package parsed wrong: %+v", p)
	}
}

func TestParseScanOutput_PhasedExcluded(t *testing.T) {
	raw := "Listing...\n" +
		"libfoo/jammy-security 1.2.3-1 amd64 [upgradable from: 1.2.2-1]\n" +
		"libphased/jammy-updates 2.0-1 amd64 [upgradable from: 1.9-1] (phased 20%)\n"
	res := parseScanOutput(raw, "h")
	if len(res.Packages) != 1 {
		t.Fatalf("phased update should be excluded; got %d packages: %+v", len(res.Packages), res.Packages)
	}
	if res.Packages[0].Name != "libfoo" {
		t.Fatalf("wrong package kept: %+v", res.Packages[0])
	}
}

func TestParseScanOutput_RebootAndPackages(t *testing.T) {
	raw := "Listing...\n" +
		"__REBOOT__\n" +
		"__REBOOT_PKGS_START__\n" +
		"linux-image-generic\n" +
		"libc6\n" +
		"__REBOOT_PKGS_END__\n"
	res := parseScanOutput(raw, "h")
	if !res.NeedsReboot {
		t.Fatal("NeedsReboot should be true when __REBOOT__ present")
	}
	if res.RebootReason != "linux-image-generic, libc6" {
		t.Fatalf("RebootReason = %q", res.RebootReason)
	}
}

func TestParseScanOutput_Needrestart(t *testing.T) {
	withSvc := "NEEDRESTART-SVC: apache2.service\nNEEDRESTART-SVC: cron.service\n"
	res := parseScanOutput(withSvc, "h")
	if !res.NeedrestartFound {
		t.Fatal("NeedrestartFound should be true when marker absent")
	}
	if len(res.NeedsRestart) != 2 || res.NeedsRestart[0] != "apache2.service" || res.NeedsRestart[1] != "cron.service" {
		t.Fatalf("NeedsRestart parsed wrong (expect trimmed names): %+v", res.NeedsRestart)
	}

	missing := "__NEEDRESTART_MISSING__\n"
	res2 := parseScanOutput(missing, "h")
	if res2.NeedrestartFound {
		t.Fatal("NeedrestartFound should be false when __NEEDRESTART_MISSING__ present")
	}

	// The per-user manager pseudo-unit (systemd-user) is not a restartable unit and must
	// be filtered out so it doesn't show as an actionable "needs restart" service.
	withUserMgr := "NEEDRESTART-SVC: apache2.service\nNEEDRESTART-SVC: systemd-user\nNEEDRESTART-SVC: cron.service\n"
	res3 := parseScanOutput(withUserMgr, "h")
	if len(res3.NeedsRestart) != 2 || res3.NeedsRestart[0] != "apache2.service" || res3.NeedsRestart[1] != "cron.service" {
		t.Fatalf("systemd-user should be filtered from NeedsRestart: %+v", res3.NeedsRestart)
	}
}

func TestParseScanOutput_AptUpdateFailed(t *testing.T) {
	ok := parseScanOutput("libfoo/jammy 1.2 amd64 [upgradable from: 1.1]\n", "h")
	if ok.AptUpdateFailed {
		t.Fatal("AptUpdateFailed should be false when marker absent")
	}
	failed := parseScanOutput("__APT_UPDATE_FAILED__\nlibfoo/jammy 1.2 amd64 [upgradable from: 1.1]\n", "h")
	if !failed.AptUpdateFailed {
		t.Fatal("AptUpdateFailed should be true when __APT_UPDATE_FAILED__ present")
	}
	// A failed apt-get update must NOT stop package parsing (uses cached lists).
	if len(failed.Packages) != 1 {
		t.Fatalf("packages should still parse after update failure; got %d", len(failed.Packages))
	}
}

func TestParseScanOutput_Sysinfo(t *testing.T) {
	raw := "libfoo/jammy 1.2 amd64 [upgradable from: 1.1]\n" +
		"__SYSINFO_START__\n" +
		"PRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\n" +
		"VERSION_ID=\"22.04\"\n" +
		"__SYSINFO_SEP__\n" +
		"up 3 days, 4 hours\n" +
		"__SYSINFO_SEP__\n" +
		"5.15.0-101-generic\n" +
		"__SYSINFO_END__\n"
	res := parseScanOutput(raw, "h")
	if res.OsName != "Ubuntu 22.04.4 LTS" {
		t.Fatalf("OsName = %q", res.OsName)
	}
	if res.OsVersion != "22.04" {
		t.Fatalf("OsVersion = %q", res.OsVersion)
	}
	if res.Uptime != "3 days, 4 hours" {
		t.Fatalf("Uptime = %q", res.Uptime)
	}
	if res.Kernel != "5.15.0-101-generic" {
		t.Fatalf("Kernel = %q", res.Kernel)
	}
	// sysinfo block must be stripped before package parsing — no stray packages.
	if len(res.Packages) != 1 {
		t.Fatalf("got %d packages, want 1 (sysinfo must not leak): %+v", len(res.Packages), res.Packages)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\nd\n", 2); got != "c | d" {
		t.Fatalf("lastLines = %q, want 'c | d'", got)
	}
	if got := lastLines("   ", 3); got != "" {
		t.Fatalf("lastLines(blank) = %q, want empty", got)
	}
}

func TestApplyStartScriptSupportsRPMManagers(t *testing.T) {
	s := applyStartScript("20261103T051200Z-abcd1234")

	for _, want := range []string{
		"dnf -y --refresh upgrade",
		"yum -y update",
		"apt-get -y",
		"__PD_UNSUPPORTED_PM__",
		"nohup setsid sh -c",
		"tail -n +1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("apply script lacks %q", want)
		}
	}
}

func TestCountChangedPackages(t *testing.T) {
	log := `Setting up nginx (1.2)
Setting up openssl (3.0)
Upgraded: kernel
Installed: curl
Updated: systemd
`

	if got := countChangedPackages(log); got != 5 {
		t.Fatalf("countChangedPackages = %d, want 5", got)
	}
}

func TestParseRPMLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantPkg models.PackageInfo
		wantOK  bool
	}{
		{
			name: "standard package line",
			line: "nginx.x86_64  1:1.24.0-1.el9  appstream",
			wantPkg: models.PackageInfo{
				Name:       "nginx",
				Arch:       "x86_64",
				NewVersion: "1:1.24.0-1.el9",
				Source:     "appstream",
			},
			wantOK: true,
		},
		{
			name: "noarch package",
			line: "ca-certificates.noarch  2023.2.60_9.0.68-72.el7_9  updates",
			wantPkg: models.PackageInfo{
				Name:       "ca-certificates",
				Arch:       "noarch",
				NewVersion: "2023.2.60_9.0.68-72.el7_9",
				Source:     "updates",
			},
			wantOK: true,
		},
		{
			name: "package name with dot",
			line: "python3.11.x86_64  3.11.5-1.el9_2.1  appstream",
			wantPkg: models.PackageInfo{
				Name:       "python3.11",
				Arch:       "x86_64",
				NewVersion: "3.11.5-1.el9_2.1",
				Source:     "appstream",
			},
			wantOK: true,
		},
		{
			name:   "installed obsoleted package with @ prefix",
			line:   "    kernel.x86_64  5.14.0-362.18.1.el9_3  @baseos",
			wantOK: false,
		},
		{
			name:   "obsoleting packages header",
			line:   "Obsoleting Packages",
			wantOK: false,
		},
		{
			name:   "last metadata check header",
			line:   "Last metadata expiration check: 0:42:15 ago on Fri 21 Nov 2026 12:00:00 PM UTC.",
			wantOK: false,
		},
		{
			name:   "plugins loaded line",
			line:   "Loaded plugins: fastestmirror, ovl",
			wantOK: false,
		},
		{
			name:   "security header",
			line:   "Security: kernel-core-5.14.0-362.24.1.el9_3.x86_64 is an installed security update",
			wantOK: false,
		},
		{
			name:   "too few fields",
			line:   "nginx.x86_64 1.2",
			wantOK: false,
		},
		{
			name:   "no arch extension",
			line:   "Modular timer and other tasks",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPkg, ok := parseRPMLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("parseRPMLine(%q) ok = %v, want %v", tc.line, ok, tc.wantOK)
			}
			if ok && gotPkg != tc.wantPkg {
				t.Fatalf("parseRPMLine(%q) = %+v, want %+v", tc.line, gotPkg, tc.wantPkg)
			}
		})
	}
}

func TestParseScanOutput_RPMPackages(t *testing.T) {
	raw := "__RPM_PACKAGES_START__\n" +
		"Last metadata expiration check: 0:15:30 ago.\n" +
		"nginx.x86_64  1:1.24.0-1.el9  appstream\n" +
		"curl.x86_64   7.76.1-26.el9_3  baseos\n" +
		"Obsoleting Packages\n" +
		"kernel.x86_64  5.14.0-362.24.1.el9_3  baseos\n" +
		"    kernel.x86_64  5.14.0-362.18.1.el9_3  @baseos\n" +
		"__RPM_PACKAGES_END__\n" +
		"__REBOOT__\n" +
		"__BOOTID_START__\n" +
		"12345678-1234-1234-1234-123456789abc\n" +
		"__BOOTID_END__\n" +
		"__SYSINFO_START__\n" +
		"PRETTY_NAME=\"AlmaLinux 9.3 (Shamrock Pampas Cat)\"\n" +
		"VERSION_ID=\"9.3\"\n" +
		"ID=\"almalinux\"\n" +
		"ID_LIKE=\"rhel centos fedora\"\n" +
		"__SYSINFO_SEP__\n" +
		"up 12 days, 3 hours\n" +
		"__SYSINFO_SEP__\n" +
		"5.14.0-362.18.1.el9_3.x86_64\n" +
		"__SYSINFO_END__\n"

	res := parseScanOutput(raw, "rpm-host-1")

	if len(res.Packages) != 3 {
		t.Fatalf("res.Packages length = %d, want 3 (%+v)", len(res.Packages), res.Packages)
	}

	wantNames := []string{"nginx", "curl", "kernel"}
	for i, name := range wantNames {
		if res.Packages[i].Name != name {
			t.Errorf("pkg[%d].Name = %q, want %q", i, res.Packages[i].Name, name)
		}
	}

	if !res.NeedsReboot {
		t.Error("res.NeedsReboot should be true")
	}
	if res.OsName != "AlmaLinux 9.3 (Shamrock Pampas Cat)" {
		t.Errorf("res.OsName = %q", res.OsName)
	}
	if res.OsVersion != "9.3" {
		t.Errorf("res.OsVersion = %q", res.OsVersion)
	}
	if res.Uptime != "12 days, 3 hours" {
		t.Errorf("res.Uptime = %q", res.Uptime)
	}
	if res.Kernel != "5.14.0-362.18.1.el9_3.x86_64" {
		t.Errorf("res.Kernel = %q", res.Kernel)
	}
}

func TestParseScanOutput_PkgUpdateFailed(t *testing.T) {
	raw := "__PKG_UPDATE_FAILED__\n" +
		"__RPM_PACKAGES_START__\n" +
		"curl.x86_64 7.76.1-26.el9_3 baseos\n" +
		"__RPM_PACKAGES_END__\n"

	res := parseScanOutput(raw, "h")
	if !res.AptUpdateFailed {
		t.Fatal("AptUpdateFailed should be true when __PKG_UPDATE_FAILED__ is present")
	}
	if len(res.Packages) != 1 {
		t.Fatalf("got %d packages, want 1", len(res.Packages))
	}
}

func TestScanCommandSupportsRPM(t *testing.T) {
	cmd := scanCommand()

	for _, want := range []string{
		"case \"$PM\" in",
		"command -v dnf",
		"command -v yum",
		"command -v apt-get",
		"__RPM_PACKAGES_START__",
		"check-update",
		"__PKG_UPDATE_FAILED__",
		"needs-restarting -r",
		"__UNSUPPORTED_PACKAGE_MANAGER__",
		"__STOPPED_START__",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("scanCommand lacks %q", want)
		}
	}
}

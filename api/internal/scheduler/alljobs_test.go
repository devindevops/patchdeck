package scheduler

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"patchdeck/api/internal/db"
	"patchdeck/api/internal/models"
)

// TestAllHostsResolvesAtRunTime: an all-hosts job runs on the fleet as it is when it fires,
// including a host added after the job was created (the v2.12.0 bug fix).
func TestAllHostsResolvesAtRunTime(t *testing.T) {
	conn, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatal(err)
	}
	add := func(name string) {
		if _, err := db.CreateHost(conn, models.Host{Name: name, Address: name + ".local", Port: 22, SSHUser: "root", AuthType: "password"}); err != nil {
			t.Fatal(err)
		}
	}
	add("a")
	e := &Engine{db: conn}
	job := models.Job{AllHosts: true}
	add("b") // added after the job was made
	hosts, err := e.resolveJobHosts(job)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("all-hosts job resolved %v, want a,b", names)
	}
}

// TestScheduledModeOptOut: the run-time rule behind "Skip this host in scheduled applies"
// (v2.13.0). An apply job skips the host, a scan + apply job only scans it, a scan job is unchanged.
func TestScheduledModeOptOut(t *testing.T) {
	out := models.Host{ChecksEnabled: true, SkipScheduledApply: true}
	in := models.Host{ChecksEnabled: true}
	for _, tc := range []struct {
		mode string
		h    models.Host
		want string
	}{
		{"apply", out, ""}, {"scan_apply", out, "scan"}, {"scan", out, "scan"},
		{"apply", in, "apply"}, {"scan_apply", in, "scan_apply"},
		{"scan", models.Host{ChecksEnabled: false}, ""},
	} {
		if got, why := scheduledMode(tc.mode, tc.h); got != tc.want {
			t.Errorf("scheduledMode(%s, skip=%v checks=%v) = %q (%s), want %q", tc.mode, tc.h.SkipScheduledApply, tc.h.ChecksEnabled, got, why, tc.want)
		}
	}
}

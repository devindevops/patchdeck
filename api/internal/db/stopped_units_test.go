package db

import (
	"testing"

	"patchdeck/api/internal/models"
)

// v2.13.5: a stopped service stays until a scan finds it running again (or it's dismissed); a scan
// that couldn't tell (no stopped block) clears nothing; deleting the host forgets it.
func TestStoppedUnitsLifecycle(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()
	h := mkHost(t, d, "svc")
	units := func() []string {
		t.Helper()
		all, err := ListStoppedUnits(d)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range all[h] {
			out = append(out, u.Unit+"/"+u.Cause)
		}
		return out
	}
	if err := AddStoppedUnits(d, h, "apply", []string{"nginx.service", "cron.service"}); err != nil {
		t.Fatal(err)
	}
	_ = AddStoppedUnits(d, h, "restart", []string{"nginx.service"}) // stopped again: latest cause + time
	assertServices(t, "recorded", units(), []string{"cron.service/apply", "nginx.service/restart"})

	// A scan that couldn't list stopped units clears nothing.
	if err := UpsertScanResult(d, h, models.ScanResult{BootID: "b1"}); err != nil {
		t.Fatal(err)
	}
	assertServices(t, "unknown scan", units(), []string{"cron.service/apply", "nginx.service/restart"})

	// cron runs again; nginx is still failed (plus an unrelated failed unit Patchdeck never tracks).
	// Within the grace a scan clears nothing (a crash-looping unit may be mid-restart).
	scan := func() {
		t.Helper()
		if err := UpsertScanResult(d, h, models.ScanResult{BootID: "b1", StoppedKnown: true, StoppedUnits: []string{"nginx.service", "old-broken.service"}}); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	assertServices(t, "scan within the grace", units(), []string{"cron.service/apply", "nginx.service/restart"})
	g := StoppedClearGrace
	StoppedClearGrace = 0
	defer func() { StoppedClearGrace = g }()
	scan()
	assertServices(t, "after scan", units(), []string{"nginx.service/restart"})

	if err := DeleteStoppedUnit(d, h, "nginx.service"); err != nil {
		t.Fatal(err)
	}
	assertServices(t, "dismissed", units(), nil)

	_ = AddStoppedUnits(d, h, "restart", []string{"x.service"})
	if err := DeleteHost(d, h); err != nil {
		t.Fatal(err)
	}
	all, _ := ListStoppedUnits(d)
	if len(all[h]) != 0 {
		t.Fatalf("host delete left %v", all[h])
	}
}

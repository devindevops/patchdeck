package db

import (
	"testing"
	"time"

	"patchdeck/api/internal/models"
)

// TestConvertAllHostJobs: before v2.12 "All hosts" saved the host list at creation. A job whose
// list covers every current host becomes a real all-hosts job (once); a partial list, a tag job
// and a single-host job are left alone.
func TestConvertAllHostJobs(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()
	a, b := mkHost(t, d, "a"), mkHost(t, d, "b")
	for _, j := range []models.Job{
		{Name: "every", CronExpr: "0 3 * * *", Mode: "scan", HostIDs: []string{a, b}, Enabled: true},
		{Name: "some", CronExpr: "0 3 * * *", Mode: "scan", HostIDs: []string{a}, Enabled: true},
		{Name: "tag", CronExpr: "0 3 * * *", Mode: "scan", TagFilter: "web", Enabled: true},
		{Name: "one", CronExpr: "0 3 * * *", Mode: "scan", HostID: b, Enabled: true},
	} {
		if err := CreateJob(d, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`DELETE FROM app_settings WHERE key='jobs_all_hosts_converted'`); err != nil {
		t.Fatal(err)
	}
	if err := convertAllHostJobs(d); err != nil {
		t.Fatal(err)
	}
	js := jobsByName(t, d)
	if !js["every"].AllHosts || len(js["every"].HostIDs) != 0 {
		t.Errorf("every: %+v, want all_hosts with no saved list", js["every"])
	}
	for _, n := range []string{"some", "tag", "one"} {
		if js[n].AllHosts {
			t.Errorf("%s must not be converted", n)
		}
	}
	// One-time: a job made later that happens to cover every host stays as made.
	if err := CreateJob(d, models.Job{Name: "later", CronExpr: "0 3 * * *", Mode: "scan", HostIDs: []string{a, b}}); err != nil {
		t.Fatal(err)
	}
	if err := convertAllHostJobs(d); err != nil {
		t.Fatal(err)
	}
	if jobsByName(t, d)["later"].AllHosts {
		t.Error("the conversion must run only once")
	}
}

// TestConvertOldAllHostsJobMissingNewHosts: the bug's own case. "All hosts" was picked when the
// fleet was {a, b}; c was added later. The list still equals every host that existed at the job's
// first run, so it converts (and c is scheduled from now on).
func TestConvertOldAllHostsJobMissingNewHosts(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()
	a, b := mkHost(t, d, "a"), mkHost(t, d, "b")
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := d.Exec(`UPDATE hosts SET created_at=? WHERE id IN (?,?)`, old, a, b); err != nil {
		t.Fatal(err)
	}
	if err := CreateJob(d, models.Job{Name: "old", CronExpr: "0 3 * * *", Mode: "scan", HostIDs: []string{a, b}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := CreateJob(d, models.Job{Name: "picked", CronExpr: "0 3 * * *", Mode: "scan", HostIDs: []string{a}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"old", "picked"} {
		if _, err := d.Exec(`INSERT INTO job_runs(id,job_id,mode,status,started_at) VALUES(?,?,?,?,?)`, n+"-run", jobsByName(t, d)[n].ID, "scan", "success", old.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	mkHost(t, d, "c") // added after the jobs first ran
	if _, err := d.Exec(`DELETE FROM app_settings WHERE key='jobs_all_hosts_converted'`); err != nil {
		t.Fatal(err)
	}
	if err := convertAllHostJobs(d); err != nil {
		t.Fatal(err)
	}
	js := jobsByName(t, d)
	if !js["old"].AllHosts {
		t.Error("an old All hosts job that predates host c should convert")
	}
	if js["picked"].AllHosts {
		t.Error("a job that targeted only some of the hosts at its first run must not convert")
	}
}

// TestAllHostsJobSurvivesHostDelete: an all-hosts job has no saved list, so deleting a host never
// prunes (or deletes) it.
func TestAllHostsJobSurvivesHostDelete(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()
	a := mkHost(t, d, "a")
	if err := CreateJob(d, models.Job{Name: "all", CronExpr: "0 3 * * *", Mode: "scan", AllHosts: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteHost(d, a); err != nil {
		t.Fatal(err)
	}
	if j, ok := jobsByName(t, d)["all"]; !ok || !j.AllHosts {
		t.Errorf("all-hosts job should survive a host delete: %+v (present=%v)", j, ok)
	}
}

// TestUpdateJob: an edit changes name, schedule, mode and target, and keeps enabled + identity.
func TestUpdateJob(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()
	a := mkHost(t, d, "a")
	if err := CreateJob(d, models.Job{Name: "j", CronExpr: "0 3 * * *", Mode: "scan", HostID: a, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	j := jobsByName(t, d)["j"]
	if err := UpdateJob(d, models.Job{ID: j.ID, Name: "j2", CronExpr: "0 4 * * 0", Mode: "scan_apply", AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	got, err := GetJob(d, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "j2" || got.CronExpr != "0 4 * * 0" || got.Mode != "scan_apply" || !got.AllHosts || got.HostID != "" || got.Enabled {
		t.Errorf("after edit: %+v (enabled must stay false)", got)
	}
	if err := UpdateJob(d, models.Job{ID: "nope", CronExpr: "0 3 * * *"}); err == nil {
		t.Error("editing a missing job should fail")
	}
}

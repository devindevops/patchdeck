package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"patchdeck/api/internal/db"
	"patchdeck/api/internal/models"
	"patchdeck/api/internal/scheduler"
)

// spike_schedules.go — scheduled jobs (cron scan/apply): create, edit, pause, delete, run history.
// Reuses the db job functions and the same cron + host-control validation the JSON API enforces.
// A job targets one host, several hosts, a tag, or all hosts (the whole fleet, resolved at every
// run — v2.12.0).

// loadJobs lists jobs with their computed last/next run, mirroring the JSON listJobs, plus
// resolving a single-host job's host name for display.
func (a *app) loadJobs() []models.Job {
	jobs, err := db.ListJobs(a.db)
	if err != nil {
		return nil
	}
	now := time.Now()
	for i := range jobs {
		if jobs[i].HostID != "" && strings.TrimSpace(jobs[i].HostName) == "" {
			if h, e := db.GetHost(a.db, jobs[i].HostID); e == nil {
				jobs[i].HostName = h.Name
			}
		}
		if lr, e := db.GetLastJobRun(a.db, jobs[i].ID); e == nil {
			jobs[i].LastRun = lr
		}
		jobs[i].NextRun = scheduler.NextRun(jobs[i].CronExpr, now)
	}
	return jobs
}

// schedForm is what the create / edit form shows: the field values, the ticked hosts, and (when
// editing) which job it saves.
type schedForm struct {
	Values   map[string]string
	Selected map[string]bool
	EditID   string
}

func (a *app) renderSchedules(w http.ResponseWriter, errMsg string, f schedForm) {
	hosts, _ := db.ListHosts(a.db)
	tags, _ := db.ListAllTags(a.db)
	if f.Values == nil {
		f.Values = map[string]string{}
	}
	a.renderNext(w, "schedules.html", map[string]any{
		"Jobs": a.loadJobs(), "Hosts": hosts, "Tags": tags, "Err": errMsg,
		"Form": f.Values, "Selected": f.Selected, "EditID": f.EditID,
	})
}

// nextScheduleRuns renders the recent run history for one job (expand-in-place).
func (a *app) nextScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runs, _ := db.ListJobRuns(a.db, id, 10)
	a.renderNext(w, "jobruns", map[string]any{"ID": id, "Runs": runs})
}

func (a *app) nextSchedules(w http.ResponseWriter, r *http.Request) {
	a.renderSchedules(w, "", schedForm{})
}

// jobTarget names a job's target for the form: all, tag, multi or one.
func jobTarget(j models.Job) string {
	switch {
	case j.AllHosts:
		return "all"
	case j.TagFilter != "":
		return "tag"
	case len(j.HostIDs) > 1:
		return "multi"
	}
	return "one"
}

// nextScheduleEdit shows the form filled in with an existing job.
func (a *app) nextScheduleEdit(w http.ResponseWriter, r *http.Request) {
	j, err := db.GetJob(a.db, chi.URLParam(r, "id"))
	if err != nil {
		http.Redirect(w, r, "/schedules", http.StatusFound)
		return
	}
	target := jobTarget(j)
	hostID := j.HostID
	if hostID == "" && len(j.HostIDs) == 1 {
		hostID = j.HostIDs[0]
	}
	sel := map[string]bool{}
	for _, id := range j.HostIDs {
		sel[id] = true
	}
	a.renderSchedules(w, "", schedForm{EditID: j.ID, Selected: sel, Values: map[string]string{
		"name": j.Name, "cron_expr": j.CronExpr, "mode": j.Mode, "target": target, "host_id": hostID, "tag_filter": j.TagFilter,
	}})
}

// jobFromForm validates the create / edit form into a job. On a problem it returns the message
// to show (with the form kept as typed).
func (a *app) jobFromForm(r *http.Request) (models.Job, schedForm, string) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	cron := strings.TrimSpace(r.FormValue("cron_expr"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	target := strings.TrimSpace(r.FormValue("target"))
	if target == "" {
		target = "one"
	}
	hostID := strings.TrimSpace(r.FormValue("host_id"))
	tag := strings.TrimSpace(r.FormValue("tag_filter"))
	f := schedForm{Selected: map[string]bool{}, Values: map[string]string{
		"name": name, "cron_expr": cron, "mode": mode, "target": target, "host_id": hostID, "tag_filter": tag,
	}}
	for _, id := range r.Form["host_ids"] {
		if id = strings.TrimSpace(id); id != "" {
			f.Selected[id] = true
		}
	}

	if cron == "" {
		return models.Job{}, f, "A cron expression is required (e.g. 0 3 * * *)."
	}
	if msg := validateCronExpression(cron); msg != "" {
		return models.Job{}, f, msg
	}
	if mode == "" {
		mode = "scan"
	}
	if mode != "scan" && mode != "apply" && mode != "scan_apply" {
		return models.Job{}, f, "Mode must be scan, apply, or scan + apply."
	}

	job := models.Job{Name: name, CronExpr: cron, Mode: mode, Enabled: true}
	switch target {
	case "tag":
		if tag == "" {
			return models.Job{}, f, "Pick a tag to target."
		}
		job.TagFilter = tag
	case "multi":
		for _, id := range r.Form["host_ids"] {
			if id = strings.TrimSpace(id); id != "" {
				job.HostIDs = append(job.HostIDs, id)
			}
		}
		if len(job.HostIDs) == 0 {
			return models.Job{}, f, "Pick at least one host."
		}
	case "all":
		// The whole fleet, looked up at every run — hosts added later are included.
		job.AllHosts = true
	default: // one host
		if hostID == "" {
			return models.Job{}, f, "Pick a host to run this on."
		}
		host, err := db.GetHost(a.db, hostID)
		if err != nil {
			return models.Job{}, f, "That host no longer exists."
		}
		if mode == "apply" || mode == "scan_apply" {
			if msg := validateJobModeAgainstHostControls(host, "apply"); msg != "" {
				return models.Job{}, f, msg
			}
		}
		job.HostID = hostID
	}
	return job, f, ""
}

func (a *app) nextScheduleCreate(w http.ResponseWriter, r *http.Request) {
	job, f, msg := a.jobFromForm(r)
	if msg != "" {
		a.renderSchedules(w, msg, f)
		return
	}
	if err := db.CreateJob(a.db, job); err != nil {
		a.renderSchedules(w, "Failed to create the schedule.", f)
		return
	}
	w.Header().Set("HX-Redirect", "/schedules")
	w.WriteHeader(http.StatusOK)
}

// nextScheduleUpdate saves an edited job (its enabled state and run history are kept).
func (a *app) nextScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, f, msg := a.jobFromForm(r)
	f.EditID = id
	if msg != "" {
		a.renderSchedules(w, msg, f)
		return
	}
	job.ID = id
	if err := db.UpdateJob(a.db, job); err != nil {
		a.renderSchedules(w, "Failed to save the schedule (it may have been deleted).", f)
		return
	}
	w.Header().Set("HX-Redirect", "/schedules")
	w.WriteHeader(http.StatusOK)
}
func (a *app) nextScheduleToggle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = r.ParseForm()
	if err := db.UpdateJobEnabled(a.db, id, r.FormValue("enabled") == "1"); err != nil {
		w.Header().Set("HX-Redirect", "/schedules")
		w.WriteHeader(http.StatusOK)
		return
	}
	for _, j := range a.loadJobs() {
		if j.ID == id {
			a.renderNext(w, "jobcard", j)
			return
		}
	}
	w.Header().Set("HX-Redirect", "/schedules")
	w.WriteHeader(http.StatusOK)
}

func (a *app) nextScheduleDelete(w http.ResponseWriter, r *http.Request) {
	_ = db.DeleteJob(a.db, chi.URLParam(r, "id"))
	w.Header().Set("HX-Redirect", "/schedules")
	w.WriteHeader(http.StatusOK)
}

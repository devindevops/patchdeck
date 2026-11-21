package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"patchdeck/api/internal/db"
	"patchdeck/api/internal/models"
	"patchdeck/api/internal/notify"
	"patchdeck/api/internal/sshx"
	"patchdeck/api/internal/tasks"
)

// spike_stopped.go — services that stopped after Patchdeck's own apply or restart (v2.13.5).
// The apply script and the restart task check 30 s afterwards (sshx.StoppedCheckDelay); what
// stopped is recorded (db stopped_units), logged, notified, and shown on the card until a scan
// finds it running again or the operator dismisses it.

// stoppedMessage is the one-line description used in the activity log and notifications.
func stoppedMessage(cause string, units []string) string {
	if cause == "restart" {
		return fmt.Sprintf("%s stopped again within %d s of being restarted", strings.Join(units, ", "), sshx.StoppedCheckSecs())
	}
	return strings.Join(units, ", ") + " stopped after the update"
}

// recordStopped keeps the stopped services on the card, logs them, and notifies (gated like a
// failed apply: the "Apply failed" switch, global + per host). alreadyAlerted: the same action
// already sent a failure (a failed apply), so per-host delivery doesn't push a second message;
// digest delivery still lists it.
func (a *app) recordStopped(r *tasks.Run, host models.Host, cause string, units []string, alreadyAlerted bool) {
	if len(units) == 0 {
		return
	}
	_ = db.AddStoppedUnits(a.db, host.ID, cause, units)
	msg := stoppedMessage(cause, units)
	_ = db.RecordActivity(a.db, host.ID, host.Name, "service_fail", msg)
	push := "Patchdeck: " + host.Name + ": " + msg
	if alreadyAlerted {
		push = ""
	}
	a.notifyHost(r, host.ID, "auto_apply_failure", push, func(d *notify.Digest) {
		d.Failed = append(d.Failed, notify.FailedHost{Name: host.Name, Phase: "services", Err: msg})
	})
}

// noteStoppedAfterApply shows the services an apply left stopped in the task's panel and records them.
func (a *app) noteStoppedAfterApply(r *tasks.Run, host models.Host, units []string, applyFailed bool) {
	for _, svc := range units {
		r.Line("err", "✗ "+svc+" stopped after the update (it wasn't failed before)")
	}
	a.recordStopped(r, host, "apply", units, applyFailed)
}

// stoppedAmong returns the units in units that the host reports as stopped right now.
func stoppedAmong(stoppedNow, units []string) []string {
	now := make(map[string]bool, len(stoppedNow))
	for _, u := range stoppedNow {
		now[u] = true
	}
	var out []string
	for _, u := range units {
		if now[u] {
			out = append(out, u)
		}
	}
	return out
}

// stoppedUnitParam reads ?unit= and checks it's exactly one of the host's recorded stopped
// services. (Not validServiceName: systemd's escaped names, like systemd-fsck@dev-disk-by\x2duuid-…,
// must still be dismissable. The retry checks validServiceName itself before any shell use.)
func (a *app) stoppedUnitParam(r *http.Request, hostID string) (string, bool) {
	unit := r.URL.Query().Get("unit")
	if unit == "" {
		return "", false
	}
	all, err := db.ListStoppedUnits(a.db)
	if err != nil {
		return "", false
	}
	for _, u := range all[hostID] {
		if u.Unit == unit {
			return unit, true
		}
	}
	return "", false
}

// stoppedNote is a retry result kept for the host page. A retry holds the host's lock for ~30 s,
// and the host page reloads when a lock it showed is released, which drops the reply; the reloaded
// page shows the note instead. Only results reached while holding the lock are kept.
type stoppedNote struct {
	msg   string
	isErr bool
	at    time.Time
}

func (a *app) setStoppedNote(hostID, msg string, isErr bool) {
	a.stoppedNoteMu.Lock()
	defer a.stoppedNoteMu.Unlock()
	if a.stoppedNotes == nil {
		a.stoppedNotes = map[string]stoppedNote{}
	}
	a.stoppedNotes[hostID] = stoppedNote{msg: msg, isErr: isErr, at: time.Now()}
}

// takeStoppedNote returns (and forgets) a host's note if it's under 2 minutes old.
func (a *app) takeStoppedNote(hostID string) (string, bool) {
	a.stoppedNoteMu.Lock()
	defer a.stoppedNoteMu.Unlock()
	n, ok := a.stoppedNotes[hostID]
	delete(a.stoppedNotes, hostID)
	if !ok || time.Since(n.at) > 2*time.Minute {
		return "", false
	}
	return n.msg, n.isErr
}

// renderStopped re-renders the host page's stopped-services section with a result line (red when
// isErr). keep: also keep it as a note for a reloading host page (see stoppedNote).
func (a *app) renderStopped(w http.ResponseWriter, hostID, msg string, isErr, keep bool) {
	v, _, err := a.nextHostView(hostID)
	if err != nil {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	if keep && msg != "" {
		a.setStoppedNote(hostID, msg, isErr)
	}
	v.StoppedMsg, v.StoppedErr = msg, isErr
	a.renderNext(w, "stoppedsec", v)
}

// nextStoppedDismiss forgets one stopped service ("I know, leave it").
func (a *app) nextStoppedDismiss(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	unit, ok := a.stoppedUnitParam(r, id)
	if !ok {
		a.renderStopped(w, id, "", false, false)
		return
	}
	_ = db.DeleteStoppedUnit(a.db, id, unit)
	if host, err := db.GetHost(a.db, id); err == nil {
		_ = db.RecordActivity(a.db, host.ID, host.Name, "service_dismissed", unit+" dismissed (still stopped on the host, as far as Patchdeck knows)")
	}
	a.renderStopped(w, id, "Dismissed "+unit+".", false, false)
}

// nextStoppedRetry restarts one stopped service (same rules as Restart services: a needrestart
// handler where the host has one, never a live restart of a no-live-restart unit), waits 30 s and
// checks it's still running.
func (a *app) nextStoppedRetry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	unit, ok := a.stoppedUnitParam(r, id)
	if !ok {
		a.renderStopped(w, id, "", false, false)
		return
	}
	if !validServiceName(unit) {
		a.renderStopped(w, id, "Patchdeck can't restart "+unit+" (its name has characters it won't put in a command). Restart it on the host, or dismiss it.", true, false)
		return
	}
	host, err := db.GetHost(a.db, id)
	if err != nil {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	if a.tasks != nil {
		if !a.tasks.TryLock(id, "restart "+unit) {
			a.renderStopped(w, id, host.Name+" is busy with another action. Try again when it finishes.", true, false)
			return
		}
		defer a.tasks.Unlock(id)
	}
	// From here the host's lock is held, so the page may reload meanwhile: keep the result (keep=true).
	res, err := a.sshClient.RestartDeferredDetached(host, a.secrets, []string{unit})
	switch {
	case len(res.RebootRequired) > 0:
		a.renderStopped(w, id, unit+" can't be restarted on a running host; it's left for the next reboot.", true, true)
		return
	case err != nil:
		_ = db.RecordActivity(a.db, host.ID, host.Name, "restart_fail", fmt.Sprintf("Restart of %s failed: %v", unit, err))
		a.renderStopped(w, id, "Couldn't restart "+unit+": "+hostErrText(err), true, true)
		return
	case len(res.Dispatched) == 0:
		a.renderStopped(w, id, unit+" isn't a loaded unit on the host any more, so there's nothing to restart. Dismiss it if it's gone.", true, true)
		return
	}
	secs := sshx.StoppedCheckSecs()
	_ = db.RecordActivity(a.db, host.ID, host.Name, "restart_ok", fmt.Sprintf("Restarted %s (checking in %d s that it stays up)", unit, secs))
	// Not tied to the request: the host page may reload or the browser may leave meanwhile, and the
	// check must still run (the result is kept as a note for the page).
	time.Sleep(sshx.StoppedCheckDelay())
	now, err := a.sshClient.StoppedUnits(host, a.secrets)
	if err != nil {
		a.renderStopped(w, id, "Restarted "+unit+", but couldn't check it afterwards: "+hostErrText(err), true, true)
		return
	}
	if len(stoppedAmong(now, []string{unit})) > 0 {
		_ = db.AddStoppedUnits(a.db, id, "restart", []string{unit})
		_ = db.RecordActivity(a.db, host.ID, host.Name, "service_fail", stoppedMessage("restart", []string{unit}))
		a.renderStopped(w, id, fmt.Sprintf("%s stopped again within %d s. Check its logs on the host (journalctl -u %s).", unit, secs, unit), true, true)
		return
	}
	_ = db.DeleteStoppedUnit(a.db, id, unit)
	_ = db.RecordActivity(a.db, host.ID, host.Name, "restart_ok", unit+" is running again")
	a.renderStopped(w, id, unit+" is running again.", false, true)
}

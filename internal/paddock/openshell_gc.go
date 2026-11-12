package paddock

import (
	"context"
	"fmt"
	"time"
)

// Collect only the checkout, never the retained Git database, agent home,
// conversation history, or sandbox identity. Maintenance starts a stopped
// sandbox without starting an agent; it is stopped again on every exit path.
func (s Service) gcSandbox(ctx context.Context, t Task, config LifecycleConfig, apply bool) (r GCResult) {
	r = GCResult{TaskID: t.ID, Worktree: t.Worktree}
	fail := func(e error) GCResult { r.Eligible = false; r.Reason = e.Error(); return r }
	ttl, _ := time.ParseDuration(config.WorktreeTTL)
	activity, e := time.Parse(time.RFC3339Nano, t.LastActivityAt)
	if e != nil {
		return fail(fmt.Errorf("activity timestamp unavailable"))
	}
	if time.Since(activity) < ttl {
		return fail(fmt.Errorf("within retention period"))
	}
	if t.Status == "running" || t.Status == "starting" || t.Status == "preparing" {
		return fail(fmt.Errorf("task may still be running"))
	}
	if t.Provenance.Profile == "investigation" && t.OpenShell != nil && t.OpenShell.ReadOnlyGuardVersion >= 4 {
		return fail(fmt.Errorf("root-owned investigation checkout is retained in its task image; per-worktree collection is unavailable"))
	}
	release, e := s.Store.taskLease(t.ID, "process")
	if e != nil {
		return fail(e)
	}
	defer release()
	_, obs := terminalFor(t.Session).Inspect(ctx, t)
	if obs.State != "missing" && obs.State != "exited" {
		return fail(fmt.Errorf("terminal process is active or unknown"))
	}
	state, e := inspectTaskSandbox(ctx, t)
	if e != nil {
		return fail(e)
	}
	if !t.OpenShell.Ready || state.Phase != "Stopped" {
		return fail(fmt.Errorf("sandbox must be confirmed stopped before collection"))
	}
	if _, e = t.OpenShell.Config.call(ctx, "sandbox", "start", t.OpenShell.Name); e != nil {
		return fail(e)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, err := t.OpenShell.Config.call(stopCtx, "sandbox", "stop", t.OpenShell.Name); err != nil {
			r.Reason += "; maintenance sandbox could not be stopped: " + err.Error()
		}
	}()
	if e = s.updateSandboxCredential(ctx, &t, false); e != nil {
		return fail(e)
	}
	for _, check := range []func() error{
		func() error { return sandboxWorkspaceIdentity(ctx, t) },
		func() error { return cleanWorkspace(ctx, t, config) },
		func() error { return verifyConversation(ctx, t) },
		func() error { return s.noPendingWork(t.ID) },
		func() error { return pushedWorkspace(ctx, t) },
		func() error { return s.noOpenPulls(ctx, t) },
	} {
		if e = check(); e != nil {
			return fail(e)
		}
	}
	head, e := taskGit(ctx, t, "rev-parse", "HEAD")
	if e != nil {
		return fail(e)
	}
	r.Eligible = true
	r.Reason = "expired sandbox checkout; safety checks passed"
	if !apply {
		return r
	}
	if e = s.Store.update(t.ID, func(v *Task) error { v.WorkspaceCommit = head; v.WorkspaceRemovedAt = now(); return nil }); e != nil {
		return fail(e)
	}
	if e = cleanWorkspace(ctx, t, config); e != nil {
		return fail(e)
	}
	if e = s.noPendingWork(t.ID); e != nil {
		return fail(e)
	}
	current, e := taskGit(ctx, t, "rev-parse", "HEAD")
	if e != nil {
		return fail(e)
	}
	if current != head {
		return fail(fmt.Errorf("checkout changed during collection"))
	}
	probe := t
	probe.Worktree = "/sandbox"
	if _, e = sandboxOutput(ctx, probe, "git", "--git-dir=/sandbox/repository.git", "-c", "core.hooksPath=/dev/null", "worktree", "remove", "--", t.Worktree); e != nil {
		return fail(e)
	}
	r.Removed = true
	r.Reason = "removed checkout; retained Git branch and native conversation in stopped sandbox"
	return r
}

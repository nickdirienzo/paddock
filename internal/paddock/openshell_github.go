package paddock

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The sandbox uses ordinary gh. The existing host poller discovers its PR by
// exact repo/branch, without exposing the host control plane to the workload.
// Only explicitly configured engineer logins can trigger agent instructions.
func (g GitHubCoordinator) discoverSandboxPulls(ctx context.Context) error {
	tasks, e := g.Service.Store.List("")
	if e != nil {
		return e
	}
	state, e := g.Service.Store.GitHubState()
	if e != nil {
		return e
	}
	var errs []error
	for _, t := range tasks {
		if t.RuntimeName() != "openshell" || t.OpenShell == nil || !t.OpenShell.Ready || t.OpenShell.Provider == "" || len(t.OpenShell.Config.WatchAuthors) == 0 || !nativeMessageSupported(t) {
			continue
		}
		watched := false
		for _, w := range state.Watches {
			if w.TaskID == t.ID {
				watched = true
				break
			}
		}
		if watched {
			continue
		}
		identity, e := state.boundIdentity(t.OpenShell.Identity)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		api := NewGitHubAPI(identity)
		repo := t.OpenShell.Repository
		owner, _, _ := strings.Cut(repo, "/")
		q := url.Values{"state": {"open"}, "head": {owner + ":" + t.Branch}, "per_page": {"2"}}
		var pulls []PullRequest
		if e = api.request(ctx, "GET", "repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls); e != nil {
			errs = append(errs, e)
			continue
		}
		if len(pulls) != 1 {
			continue
		}
		if e = g.Watch(ctx, fmt.Sprintf("https://github.com/%s/pull/%d", repo, pulls[0].Number), t.ID, t.OpenShell.Config.WatchAuthors); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

package paddock

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Shipment is derived afresh at each destructive check, never from a poller's
// cached 'merged' display. Old explicit operator confirmations remain supported.
func (s Service) resolveShipment(ctx context.Context, t *Task) error {
	return NewGitHubCoordinator(s).resolveShipment(ctx, t)
}

func (coordinator GitHubCoordinator) resolveShipment(ctx context.Context, t *Task) error {
	s := coordinator.Service
	if t.ShippedAt != "" && t.ShipmentSource != "github" {
		return nil
	}
	state, err := s.Store.GitHubState()
	if err != nil {
		return err
	}
	raw := ""
	if t.OpenShell != nil {
		raw = t.OpenShell.Repository
	} else {
		raw, err = git(ctx, t.Repo, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
	}
	origin := raw
	if !githubRepoName.MatchString(origin) {
		origin, err = githubRepositoryURL(raw)
		if err != nil {
			return err
		}
	}
	apiFor := func(repo string) (GitHubAPI, error) {
		var pinned *GitHubIdentityBinding
		if t.OpenShell != nil {
			pinned = &t.OpenShell.Identity
		}
		remote, _, e := coordinator.remoteFor(state, repo, pinned)
		if e != nil {
			return GitHubAPI{}, e
		}
		api, ok := remote.(GitHubAPI)
		if !ok {
			return GitHubAPI{}, fmt.Errorf("shipment API unavailable")
		}
		if e = api.ValidateIdentity(ctx); e != nil {
			return GitHubAPI{}, e
		}
		return api, nil
	}
	api, err := apiFor(origin)
	if err != nil {
		return err
	}
	var info struct {
		FullName string `json:"full_name"`
		Parent   *struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if err = api.request(ctx, "GET", "repos/"+origin, nil, &info); err != nil {
		return err
	}
	if !strings.EqualFold(info.FullName, origin) {
		return fmt.Errorf("shipment repository identity mismatch")
	}
	base := origin
	if info.Parent != nil {
		base = info.Parent.FullName
	}
	if !githubRepoName.MatchString(base) {
		return fmt.Errorf("unknown shipment destination")
	}
	api, err = apiFor(base)
	if err != nil {
		return err
	}
	links := append([]TaskPullRequest(nil), t.PullRequests...)
	for _, w := range state.Watches {
		if w.TaskID == t.ID {
			links = append(links, TaskPullRequest{w.Repo, w.Number, w.HeadRepo, w.HeadRef})
		}
	}
	// Discover all PRs for the current task branch, including earlier closed PRs.
	// Bound pagination fails closed rather than accepting a partial inventory.
	owner, _, _ := strings.Cut(origin, "/")
	for page := 1; ; page++ {
		if page > 100 {
			return fmt.Errorf("shipment PR inventory pagination limit")
		}
		q := url.Values{"state": {"all"}, "head": {owner + ":" + t.Branch}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		var pulls []PullRequest
		if err = api.request(ctx, "GET", "repos/"+base+"/pulls?"+q.Encode(), nil, &pulls); err != nil {
			return err
		}
		for _, p := range pulls {
			if !strings.EqualFold(p.Head.Repo.FullName, origin) || p.Head.Ref != t.Branch {
				return fmt.Errorf("shipment PR discovery identity mismatch")
			}
			links = append(links, TaskPullRequest{base, p.Number, p.Head.Repo.FullName, p.Head.Ref})
		}
		if len(pulls) < 100 {
			break
		}
	}
	result, err := mergedShipment(ctx, *t, base, links, func(ctx context.Context, repo string, n int) (PullRequest, error) {
		a, e := apiFor(repo)
		if e != nil {
			return PullRequest{}, e
		}
		return a.Pull(ctx, repo, n)
	})
	if err != nil {
		return err
	}
	*t = result
	return nil
}

// Every linked PR must be merged into the shipping repository's main branch.
// Exact head SHA coverage works for merge, squash and rebase merges alike.
func mergedShipment(ctx context.Context, t Task, base string, links []TaskPullRequest, pull func(context.Context, string, int) (PullRequest, error)) (Task, error) {
	seen := map[string]TaskPullRequest{}
	inventory := []TaskPullRequest{}
	latest := time.Time{}
	covered := false
	latestHead := time.Time{}
	for _, link := range links {
		key := fmt.Sprintf("%s#%d", strings.ToLower(link.Repo), link.Number)
		if old, ok := seen[key]; ok {
			if !strings.EqualFold(old.HeadRepo, link.HeadRepo) || old.HeadRef != link.HeadRef {
				return t, fmt.Errorf("conflicting linked PR identities")
			}
			continue
		}
		seen[key] = link
		inventory = append(inventory, link)
		if !strings.EqualFold(link.Repo, base) || link.Number < 1 || link.HeadRef == "" || !githubRepoName.MatchString(link.HeadRepo) {
			return t, fmt.Errorf("unknown linked PR shipment destination")
		}
		p, err := pull(ctx, link.Repo, link.Number)
		if err != nil {
			return t, fmt.Errorf("linked PR check unavailable: %w", err)
		}
		if p.Number != link.Number || !strings.EqualFold(p.Base.Repo.FullName, base) || p.Base.Ref != "main" || p.Head.Ref != link.HeadRef || !strings.EqualFold(p.Head.Repo.FullName, link.HeadRepo) {
			return t, fmt.Errorf("linked PR identity or shipment destination changed")
		}
		stamp, err := time.Parse(time.RFC3339Nano, p.MergedAt)
		if !p.Merged || p.State != "closed" || err != nil || stamp.After(time.Now()) || len(p.Head.SHA) != 40 {
			return t, fmt.Errorf("linked PR %s is not verifiably merged", key)
		}
		if stamp.After(latest) {
			latest = stamp
		}
		if p.Head.Ref == t.Branch {
			// Earlier PRs can have older heads; the most recent merge must
			// cover the final checkout head, checked separately by shipmentGate.
			if !covered || stamp.After(latestHead) {
				latestHead = stamp
				t.ShippedCommit = p.Head.SHA
				t.ShippedAt = p.MergedAt
				covered = true
			}
		}
	}
	if !covered {
		return t, fmt.Errorf("no merged PR covers current task branch; operator shipment required")
	}
	t.ShippedAt = latest.UTC().Format(time.RFC3339Nano)
	t.ShipmentSource = "github"
	t.PullRequests = inventory
	return t, nil
}
func (s Service) saveShipment(t Task) error {
	return s.Store.update(t.ID, func(v *Task) error {
		v.ShippedAt = t.ShippedAt
		v.ShippedCommit = t.ShippedCommit
		v.ShipmentSource = t.ShipmentSource
		v.PullRequests = t.PullRequests
		return nil
	})
}

func shippedWorkspacePushed(ctx context.Context, t Task) error {
	if t.ShipmentSource == "github" {
		return shipmentGate(ctx, t)
	}
	return pushedWorkspace(ctx, t)
}

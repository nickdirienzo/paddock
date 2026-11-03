package paddock

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func githubRepositoryURL(raw string) (string, error) {
	var repo string
	if strings.HasPrefix(raw, "git@github.com:") {
		repo = strings.TrimPrefix(raw, "git@github.com:")
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() != "github.com" {
			return "", fmt.Errorf("cannot check PRs for non-GitHub remote")
		}
		repo = strings.TrimPrefix(u.Path, "/")
	}
	repo = strings.TrimSuffix(repo, ".git")
	if !githubRepoName.MatchString(repo) {
		return "", fmt.Errorf("invalid GitHub remote")
	}
	return repo, nil
}

func (s Service) noOpenPulls(ctx context.Context, t Task) error {
	state, err := s.Store.GitHubState()
	if err != nil {
		return err
	}
	remotes, err := git(ctx, t.Worktree, "remote")
	if err != nil {
		return err
	}
	repos, owners := map[string]bool{}, map[string]bool{}
	for _, remote := range strings.Fields(remotes) {
		raw, err := git(ctx, t.Worktree, "remote", "get-url", remote)
		if err != nil {
			return err
		}
		repo, err := githubRepositoryURL(raw)
		if err != nil {
			return err
		}
		repos[repo] = true
		owners[strings.Split(repo, "/")[0]] = true
	}
	if len(repos) == 0 {
		return fmt.Errorf("no GitHub remote; open PR status unknown")
	}
	for _, w := range state.Watches {
		if w.TaskID == t.ID {
			repos[w.Repo] = true
			owners[strings.Split(w.HeadRepo, "/")[0]] = true
		}
	}
	// Include the upstream of GitHub forks even when it has no local remote.
	bases := map[string]bool{}
	for repo := range repos {
		_, identity, err := state.ResolveGitHubIdentity(repo)
		if err != nil {
			return err
		}
		api := NewGitHubAPI(identity)
		if err := api.ValidateIdentity(ctx); err != nil {
			return err
		}
		var info struct {
			Parent *struct {
				FullName string `json:"full_name"`
			} `json:"parent"`
		}
		if err := api.request(ctx, "GET", "repos/"+repo, nil, &info); err != nil {
			return fmt.Errorf("PR repository check unavailable: %w", err)
		}
		bases[repo] = true
		if info.Parent != nil {
			bases[info.Parent.FullName] = true
		}
	}
	for repo := range bases {
		_, identity, err := state.ResolveGitHubIdentity(repo)
		if err != nil {
			return err
		}
		api := NewGitHubAPI(identity)
		if err := api.ValidateIdentity(ctx); err != nil {
			return err
		}
		for owner := range owners {
			open, err := api.HasOpenPull(ctx, repo, owner, t.Branch)
			if err != nil {
				return fmt.Errorf("open PR check unavailable: %w", err)
			}
			if open {
				return fmt.Errorf("open PR for task branch in %s", repo)
			}
		}
	}
	return nil
}

func (g GitHubAPI) HasOpenPull(ctx context.Context, repo, owner, branch string) (bool, error) {
	var pulls []PullRequest
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "per_page": {"1"}}
	err := g.request(ctx, "GET", "repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls)
	return len(pulls) != 0, err
}

package paddock

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
)

type TaskArtifacts struct {
	TaskID             string   `json:"task_id"`
	Branch             string   `json:"branch"`
	BaseCommit         string   `json:"base_commit"`
	CommitIDs          []string `json:"commit_ids"`
	UntrackedFiles     []string `json:"untracked_files"`
	Patch              string   `json:"patch,omitempty"`
	PatchError         string   `json:"patch_error,omitempty"`
	PullRequests       []string `json:"pull_requests"`
	PullRequestsError  string   `json:"pull_requests_error,omitempty"`
	WorkspaceAvailable bool     `json:"workspace_available"`
	WorkspaceError     string   `json:"workspace_error,omitempty"`
}

type branchPullLister interface {
	PullsForBranch(context.Context, string, string) ([]PullRequest, error)
}

func taskBranchPullRequests(ctx context.Context, t Task, remote branchPullLister) ([]string, error) {
	if t.OpenShell == nil || !githubRepoName.MatchString(t.OpenShell.Repository) {
		return nil, fmt.Errorf("task has no pinned GitHub repository")
	}
	pulls, err := remote.PullsForBranch(ctx, t.OpenShell.Repository, t.Branch)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, pr := range pulls {
		if pr.Number > 0 && pr.Head.Ref == t.Branch && strings.EqualFold(pr.Head.Repo.FullName, t.OpenShell.Repository) && strings.EqualFold(pr.Base.Repo.FullName, t.OpenShell.Repository) {
			urls = append(urls, fmt.Sprintf("https://github.com/%s/pull/%d", t.OpenShell.Repository, pr.Number))
		}
	}
	return urls, nil
}

// artifactGit bounds read-only Git output so a service can consume artifacts
// without importing unbounded workspace content into the host process.
func artifactGit(ctx context.Context, t Task, limit int64, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	gitArgs := append([]string{"git", "-C", t.Worktree, "-c", "core.hooksPath=" + os.DevNull}, args...)
	var cmd *exec.Cmd
	if t.RuntimeName() == "openshell" {
		cmd = sandboxExec(ctx, t, false, gitArgs...)
	} else {
		cmd = exec.CommandContext(ctx, gitArgs[0], gitArgs[1:]...)
		cmd.Env = credentialFreeEnvironment(os.Environ())
	}
	cmd.Stderr = io.Discard
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err = cmd.Start(); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(pipe, limit+1))
	if err != nil || int64(len(data)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("Git artifact exceeds %d bytes or could not be read", limit)
	}
	if err = cmd.Wait(); err != nil {
		return "", fmt.Errorf("Git artifact command failed")
	}
	return strings.TrimSpace(string(data)), nil
}

func (s Service) Artifacts(ctx context.Context, t Task) (TaskArtifacts, error) {
	r := TaskArtifacts{TaskID: t.ID, Branch: t.Branch, BaseCommit: t.BaseCommit, CommitIDs: []string{}, UntrackedFiles: []string{}, PullRequests: []string{}}
	state, err := s.Store.GitHubState()
	if err != nil {
		return r, err
	}
	for _, watch := range state.Watches {
		if watch.TaskID == t.ID {
			r.PullRequests = append(r.PullRequests, fmt.Sprintf("https://github.com/%s/pull/%d", watch.Repo, watch.Number))
		}
	}
	if t.OpenShell != nil && t.OpenShell.Provider != "" {
		identity, lookupErr := state.boundIdentity(t.OpenShell.Identity)
		if lookupErr == nil && (identity.Mode != "app" || identity.App == nil) {
			lookupErr = fmt.Errorf("task has no active GitHub App identity")
		}
		if lookupErr == nil {
			var urls []string
			urls, lookupErr = taskBranchPullRequests(ctx, t, NewGitHubAPI(identity))
			r.PullRequests = append(r.PullRequests, urls...)
		}
		if lookupErr != nil {
			r.PullRequestsError = lookupErr.Error()
		}
	}
	sort.Strings(r.PullRequests)
	r.PullRequests = slices.Compact(r.PullRequests)
	if t.WorkspaceRemovedAt != "" || (t.RuntimeName() == "openshell" && (t.OpenShell == nil || !t.OpenShell.Ready)) {
		r.WorkspaceError = "workspace is unavailable"
		return r, nil
	}
	commits, err := artifactGit(ctx, t, 256<<10, "log", "--format=%H", t.BaseCommit+"..HEAD")
	if err != nil {
		r.WorkspaceError = err.Error()
		return r, nil
	}
	r.WorkspaceAvailable = true
	if commits != "" {
		r.CommitIDs = strings.Split(commits, "\n")
	}
	untracked, err := artifactGit(ctx, t, 256<<10, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		r.WorkspaceError = err.Error()
		return r, nil
	}
	if untracked != "" {
		r.UntrackedFiles = strings.Split(untracked, "\n")
	}
	r.Patch, err = artifactGit(ctx, t, 2<<20, "diff", "--binary", "--no-ext-diff", "--no-textconv", t.BaseCommit)
	if err != nil {
		r.PatchError = err.Error()
	}
	return r, nil
}

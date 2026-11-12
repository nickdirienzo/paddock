package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OpenShellConfig contains operator-controlled, non-secret references only.
// Snapshot it in each task: selecting another gateway must not move a task.
type OpenShellConfig struct {
	WatchAuthors   []string          `json:"watch_authors,omitempty"`
	CodexAccountID string            `json:"codex_account_id,omitempty"`
	Binary         string            `json:"binary"`
	Gateway        string            `json:"gateway"`
	Workspace      string            `json:"workspace"`
	ConfigHome     string            `json:"config_home,omitempty"`
	StateHome      string            `json:"state_home,omitempty"`
	TLSDir         string            `json:"tls_dir,omitempty"`
	Image          string            `json:"image"`
	Policy         string            `json:"policy"`
	Memory         string            `json:"memory,omitempty"`
	CPU            string            `json:"cpu,omitempty"`
	Providers      map[string]string `json:"agent_providers"`
}
type OpenShellTask struct {
	AgentProxy           string                `json:"agent_proxy"`
	ID                   string                `json:"id,omitempty"`
	Config               OpenShellConfig       `json:"config"`
	Name                 string                `json:"name"`
	Provider             string                `json:"provider"`
	Repository           string                `json:"repository"`
	Identity             GitHubIdentityBinding `json:"identity"`
	GitAuthorName        string                `json:"git_author_name,omitempty"`
	GitAuthorEmail       string                `json:"git_author_email,omitempty"`
	CredentialExpiresAt  time.Time             `json:"credential_expires_at,omitempty"`
	CredentialError      string                `json:"credential_error,omitempty"`
	ReadOnlyGuard        bool                  `json:"read_only_guard,omitempty"`
	ReadOnlyGuardVersion int                   `json:"read_only_guard_version,omitempty"`
	Ready                bool                  `json:"ready"`
}

func (t Task) RuntimeName() string {
	if t.Runtime == "" {
		return "host"
	}
	return t.Runtime
}
func (s *Store) OpenShellConfig() (OpenShellConfig, error) {
	var c OpenShellConfig
	b, e := os.ReadFile(filepath.Join(s.Home, "openshell.json"))
	if errors.Is(e, os.ErrNotExist) {
		return c, fmt.Errorf("OpenShell is required; configure it with paddock runtime configure (see paddock runtime --help)")
	}
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	return c, c.validate()
}
func (c OpenShellConfig) validate() error {
	if err := validateWatchAuthors(c.WatchAuthors); err != nil {
		return err
	}
	if c.Binary == "" || !filepath.IsAbs(c.Binary) || c.Gateway == "" || c.Workspace == "" || c.Image == "" || !filepath.IsAbs(c.Policy) {
		return fmt.Errorf("OpenShell requires an absolute binary and policy path, gateway, workspace, and image")
	}
	if !strings.Contains(c.Image, "@sha256:") && !strings.HasPrefix(c.Image, "sha256:") {
		return fmt.Errorf("pin the sandbox image by digest (repository@sha256:... or a local sha256 image ID)")
	}
	for _, p := range []string{c.ConfigHome, c.StateHome, c.TLSDir} {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("OpenShell configuration directories must be absolute")
		}
	}
	return nil
}
func (s *Store) SetOpenShellConfig(c OpenShellConfig) error {
	if e := c.validate(); e != nil {
		return e
	}
	return s.withLock(func() error { return writeJSONAtomic(filepath.Join(s.Home, "openshell.json"), c) })
}
func (c OpenShellConfig) env() []string {
	// The host client needs gateway credentials, never personal Git/model credentials.
	var env []string
	for _, k := range []string{"HOME", "PATH", "TMPDIR", "SYSTEMROOT"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for _, v := range [][2]string{{"XDG_CONFIG_HOME", c.ConfigHome}, {"XDG_STATE_HOME", c.StateHome}, {"OPENSHELL_LOCAL_TLS_DIR", c.TLSDir}} {
		if v[1] != "" {
			env = append(env, v[0]+"="+v[1])
		}
	}
	return append(env, "NO_COLOR=1")
}
func (c OpenShellConfig) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Binary, append([]string{"--gateway", c.Gateway, "--workspace", c.Workspace}, args...)...)
	cmd.Env = c.env()
	return cmd
}
func (c OpenShellConfig) call(ctx context.Context, args ...string) ([]byte, error) {
	limit := 30 * time.Second
	if len(args) > 1 && args[0] == "sandbox" && (args[1] == "create" || args[1] == "upload" || args[1] == "start") {
		limit = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := c.command(ctx, args...)
	// A second Ctrl-C after the TUI exits must not interrupt its cleanup.
	// These control commands have no terminal; cancellation remains bounded.
	detachPoller(cmd)
	b, e := cmd.Output()
	// Do not include CLI stderr: credential errors can contain credential values.
	if e != nil {
		return nil, fmt.Errorf("OpenShell %s: %w; inspect the gateway (no host fallback)", strings.Join(args[:min(2, len(args))], " "), e)
	}
	return b, nil
}
func (s Service) CheckOpenShell(ctx context.Context) error {
	c, e := s.Store.OpenShellConfig()
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, e := c.call(ctx, "--version")
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != "openshell 0.1.6" {
		return fmt.Errorf("this integration requires qualified OpenShell 0.1.6; found %s", strings.TrimSpace(string(b)))
	}
	_, e = c.call(ctx, "gateway", "info")
	return e
}
func sandboxExec(ctx context.Context, t Task, tty bool, args ...string) *exec.Cmd {
	flags := []string{"sandbox", "exec", t.OpenShell.Name, "--no-login-shell", "--workdir", t.Worktree}
	if tty {
		flags = append(flags, "--tty")
	} else {
		flags = append(flags, "--no-tty")
	}
	for _, v := range []string{"TERM=xterm-256color", "HOME=/home/sandbox", "PATH=/sandbox/.paddock/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "CODEX_HOME=/home/sandbox/.codex", "CLAUDE_CONFIG_DIR=/home/sandbox/.claude", "PADDOCK_TASK_ID=" + t.ID, "GOCACHE=/sandbox/go-cache", "GOPATH=/sandbox/go-path"} {
		flags = append(flags, "--env", v)
	}
	if t.OpenShell.Config.CodexAccountID != "" {
		flags = append(flags, "--env", "CODEX_ACCOUNT_ID="+t.OpenShell.Config.CodexAccountID)
	}
	flags = append(flags, "--")
	cmd := t.OpenShell.Config.command(ctx, append(flags, args...)...)
	if !tty {
		detachPoller(cmd)
	}
	return cmd
}
func sandboxOutput(ctx context.Context, t Task, args ...string) (string, error) {
	b, e := sandboxExec(ctx, t, false, args...).Output()
	if e != nil {
		return "", fmt.Errorf("sandbox command %s failed: %w", args[0], e)
	}
	return strings.TrimSpace(string(b)), nil
}
func agentCommandContext(ctx context.Context, t Task, args ...string) *exec.Cmd {
	if t.RuntimeName() == "openshell" {
		return sandboxExec(ctx, t, false, append([]string{t.Session.AgentPath}, args...)...)
	}
	c := exec.CommandContext(ctx, t.Session.AgentPath, args...)
	c.Env = agentControlEnv(t)
	return c
}
func taskGit(ctx context.Context, t Task, args ...string) (string, error) {
	if t.RuntimeName() == "openshell" {
		return sandboxOutput(ctx, t, append([]string{"git", "-C", t.Worktree}, args...)...)
	}
	return git(ctx, t.Worktree, args...)
}

func (s Service) runOpenShell(ctx context.Context, o RunOptions, repo Repository) (Task, error) {
	var t Task
	if e := ValidateStateHome(s.Store.Home, repo.Root); e != nil {
		return t, e
	}
	c, e := s.Store.OpenShellConfig()
	if e != nil {
		return t, e
	}
	if c.Providers[o.Agent] == "" {
		return t, fmt.Errorf("configure an OpenShell %s model provider with paddock runtime configure; no host credentials are discovered automatically", o.Agent)
	}
	if e = s.CheckOpenShell(ctx); e != nil {
		return t, e
	}
	raw, remoteErr := git(ctx, repo.Root, "remote", "get-url", "origin")
	remote := ""
	if remoteErr == nil {
		remote, remoteErr = githubRepositoryURL(raw)
	}
	state, e := s.Store.GitHubState()
	if e != nil {
		return t, e
	}
	var binding GitHubIdentityBinding
	var identity GitHubIdentity
	if o.Identity != "" {
		identity, e = state.IdentityForCaller(o.Identity, o.Profile, o.provenance.Caller)
		if e == nil {
			binding = bindGitHubIdentity(o.Identity, identity)
		}
	} else {
		if remoteErr != nil {
			return t, fmt.Errorf("a repository route requires a GitHub origin: %w", remoteErr)
		}
		binding, identity, e = state.ResolveGitHubIdentity(remote)
		if e == nil {
			_, e = state.IdentityForCaller(binding.Name, o.Profile, o.provenance.Caller)
		}
	}
	if e != nil {
		return t, e
	}
	githubApp := identity.Mode == "app" && identity.App != nil
	if identity.Mode != "" && !githubApp {
		return t, fmt.Errorf("identity %q has no usable OpenShell GitHub App; detach legacy GitHub credentials to run local-only", binding.Name)
	}
	if githubApp && remoteErr != nil {
		return t, fmt.Errorf("GitHub App identity requires a GitHub origin: %w", remoteErr)
	}
	_, session, e := SelectTerminal(ctx, o.Backend)
	if e != nil {
		return t, e
	}
	id := o.ID
	if id == "" {
		id = newID()
	}
	if !validID(id) {
		return t, fmt.Errorf("invalid task ID")
	}
	// Names fit OpenShell's 19-character limit. Ownership labels prevent
	// adopting another task on a collision.
	digest := sha256.Sum256([]byte(s.Store.Home + "\x00" + id))
	name := "pd-" + hex.EncodeToString(digest[:8])
	session.Agent = o.Agent
	session.AgentPath = "/usr/local/bin/" + o.Agent
	session.Generation = 1
	session.Name = "paddock-" + id
	session.AgentHome = "/home/sandbox/." + o.Agent
	if o.Agent == "claude" {
		session.ConversationID = newConversationID()
	}
	trusted, e := RepoTrusted(s.Store.Home, repo.CommonDir)
	if e != nil {
		return t, e
	}
	session.TrustProject = trusted && o.Agent == "codex"
	o.provenance.Identity = binding.Name
	o.provenance.Capabilities = executionCapabilities(o.Profile, githubApp)
	author, mail, provider := binding.Name, binding.Name+"@paddock.invalid", ""
	if githubApp {
		o.provenance.GitHubActor = binding.Login
		botID := identity.BotUserID
		if botID == 0 {
			botID, e = NewGitHubAPI(identity).BotUserID(ctx, binding.Login)
			if e != nil {
				return t, fmt.Errorf("verify GitHub bot commit actor: %w", e)
			}
		}
		author, mail, provider = binding.Login, fmt.Sprintf("%d+%s@users.noreply.github.com", botID, binding.Login), "paddock-"+id
	} else {
		remote = ""
	}
	t = Task{ID: id, Runtime: "openshell", Provenance: o.provenance, OpenShell: &OpenShellTask{Config: c, Name: name, Provider: provider, Repository: remote, Identity: binding, GitAuthorEmail: mail, GitAuthorName: author}, ParentID: o.Parent, SignalID: o.SignalID, RelatedTaskID: o.RelatedTaskID, Prompt: o.Prompt, Repo: repo.Root, CommonDir: repo.CommonDir, Base: repo.Base, BaseCommit: repo.Commit, Branch: "paddock/" + id, Worktree: "/sandbox/worktree", Status: "preparing", CreatedAt: now(), UpdatedAt: now(), LastActivityAt: now(), Session: session}
	t.OpenShell.AgentProxy, e = sandboxAgentProxy(s.Store.Home, c.Binary, o.Agent)
	if e != nil {
		return t, e
	}
	if e = s.Store.Insert(t); e != nil {
		return t, e
	}
	releasePreparation, e := s.Store.taskLease(id, "prepare")
	if e != nil {
		return t, e
	}
	defer releasePreparation()
	fail := func(err error) (Task, error) {
		_ = s.Store.Status(id, "failed", err.Error(), "preparing", "starting")
		current, _ := s.Store.Get(id)
		return current, fmt.Errorf("task %s: %w; inspect with paddock show %s", id, err, id)
	}
	if e = s.provisionSandbox(ctx, &t); e != nil {
		return fail(e)
	}
	if e = s.Store.update(id, func(v *Task) error { v.OpenShell = t.OpenShell; v.Status = "starting"; return nil }); e != nil {
		return fail(e)
	}
	// The same singleton serves GitHub routing and credential upkeep. No per-task daemon.
	if e = s.ensureRuntimePoller(ctx); e != nil {
		return fail(e)
	}
	return s.launchTask(ctx, t)
}

func githubProfile(repo, id string) map[string]any {
	return githubProfileForExecution(repo, id, "implementation")
}

func githubProfileForExecution(repo, id, profile string) map[string]any {
	rule := func(method, path string) any {
		return map[string]any{"allow": map[string]string{"method": method, "path": path}}
	}
	apiRules := []any{rule("GET", "/repos/"+repo), rule("GET", "/repos/"+repo+"/**"), rule("GET", "/installation/repositories")}
	gitRules := []any{rule("GET", "/"+repo+".git/**"), rule("POST", "/"+repo+".git/git-upload-pack")}
	graphqlAccess := "read-only"
	if profile == "implementation" {
		apiRules = append(apiRules, rule("POST", "/repos/"+repo+"/**"), rule("PATCH", "/repos/"+repo+"/**"), rule("DELETE", "/repos/"+repo+"/git/refs/**"))
		gitRules = append(gitRules, rule("POST", "/"+repo+".git/git-receive-pack"))
		graphqlAccess = "read-write"
	}
	return map[string]any{"id": id, "display_name": "Paddock task GitHub identity", "category": "source_control",
		"credentials": []any{map[string]any{"name": "api_token", "env_vars": []string{"GH_TOKEN", "GITHUB_TOKEN"}, "required": true, "stable_placeholder": true, "auth_style": "bearer", "header_name": "authorization"}},
		"endpoints": []any{
			map[string]any{"host": "api.github.com", "port": 443, "protocol": "rest", "enforcement": "enforce", "rules": apiRules},
			map[string]any{"host": "api.github.com", "port": 443, "path": "/graphql", "protocol": "graphql", "access": graphqlAccess, "enforcement": "enforce"},
			map[string]any{"host": "github.com", "port": 443, "path": "/" + repo + ".git/**", "protocol": "rest", "enforcement": "enforce", "rules": gitRules}},
		"binaries": []string{"/usr/bin/git", "/usr/bin/gh"}}
}

// Fresh tokens are never shared with the poller's broad installation transport.
func (a *githubAppAuth) repositoryToken(ctx context.Context, repo string) (string, time.Time, error) {
	return a.repositoryTokenForExecution(ctx, repo, "implementation")
}

func (a *githubAppAuth) repositoryTokenForExecution(ctx context.Context, repo, profile string) (string, time.Time, error) {
	if !githubRepoName.MatchString(repo) {
		return "", time.Time{}, fmt.Errorf("invalid token repository")
	}
	jwt, e := a.jwt()
	if e != nil {
		return "", time.Time{}, e
	}
	var result struct {
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
		Permissions map[string]string `json:"permissions"`
	}
	permissions := map[string]string{"contents": "write", "pull_requests": "write", "issues": "write"}
	if profile == "investigation" {
		permissions = map[string]string{"contents": "read", "pull_requests": "read", "issues": "read"}
	}
	_, name, _ := strings.Cut(repo, "/")
	_, e = a.http(ctx, "POST", fmt.Sprintf("app/installations/%d/access_tokens", a.config.InstallationID), jwt, map[string]any{"repositories": []string{name}, "permissions": permissions}, &result)
	if e != nil {
		return "", time.Time{}, e
	}
	if result.Token == "" || !result.ExpiresAt.After(a.clock().Add(5*time.Minute)) || len(result.Repositories) != 1 || !strings.EqualFold(result.Repositories[0].FullName, repo) {
		return "", time.Time{}, fmt.Errorf("GitHub did not return a valid token restricted to %s", repo)
	}
	for k, v := range result.Permissions {
		if k == "metadata" && v == "read" {
			continue
		}
		if permissions[k] != v {
			return "", time.Time{}, fmt.Errorf("GitHub returned unexpected token permissions")
		}
	}
	for k, v := range permissions {
		if result.Permissions[k] != v {
			return "", time.Time{}, fmt.Errorf("GitHub App needs %s: %s for this runtime", k, v)
		}
	}
	return result.Token, result.ExpiresAt, nil
}
func (s Service) sandboxToken(ctx context.Context, t Task) (string, time.Time, error) {
	if t.OpenShell == nil || t.OpenShell.Provider == "" {
		return "", time.Time{}, fmt.Errorf("task has no GitHub credential provider")
	}
	state, e := s.Store.GitHubState()
	if e != nil {
		return "", time.Time{}, e
	}
	i, e := state.boundIdentity(t.OpenShell.Identity)
	if e != nil {
		return "", time.Time{}, e
	}
	if i.App == nil {
		return "", time.Time{}, fmt.Errorf("task identity no longer has an App")
	}
	return newGitHubAppAuth(*i.App).repositoryTokenForExecution(ctx, t.OpenShell.Repository, t.Provenance.Profile)
}
func (s Service) updateSandboxCredential(ctx context.Context, t *Task, create bool) error {
	release, err := s.Store.taskLease(t.ID, "credentials")
	if err != nil {
		return err
	}
	defer release()
	token, expiry, e := s.sandboxToken(ctx, *t)
	if e != nil {
		return e
	}
	c := t.OpenShell.Config
	args := []string{"provider", "update", t.OpenShell.Provider, "--credential", "GH_TOKEN", "--credential-expires-at", "GH_TOKEN=" + expiry.Format(time.RFC3339), "--wait", "--timeout", "30"}
	if create {
		args = []string{"provider", "create", "--name", t.OpenShell.Provider, "--type", t.OpenShell.Provider, "--credential", "GH_TOKEN"}
	}
	cmd := c.command(ctx, args...)
	cmd.Env = append(cmd.Env, "GH_TOKEN="+token)
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("could not %s task credential provider (no credentials logged)", args[1])
	}
	if create { // Create has no expiry flag. Set it before any workload can attach.
		_, e = c.call(ctx, "provider", "update", t.OpenShell.Provider, "--credential-expires-at", "GH_TOKEN="+expiry.Format(time.RFC3339))
		if e != nil {
			return e
		}
	}
	t.OpenShell.CredentialExpiresAt = expiry
	t.OpenShell.CredentialError = ""
	return nil
}
func (s Service) provisionSandbox(ctx context.Context, t *Task) error {
	c := t.OpenShell.Config
	dir, e := os.MkdirTemp(s.Store.Home, ".sandbox-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	if t.OpenShell.Provider != "" {
		profile := filepath.Join(dir, "github.json")
		if e = writeJSONAtomic(profile, githubProfileForExecution(t.OpenShell.Repository, t.OpenShell.Provider, t.Provenance.Profile)); e != nil {
			return e
		}
		if _, e = c.call(ctx, "profile", "lint", "--file", profile); e != nil {
			return e
		}
		if _, e = c.call(ctx, "profile", "import", "--file", profile); e != nil {
			return e
		}
		if e = s.updateSandboxCredential(ctx, t, true); e != nil {
			return e
		}
	}
	// Retain the workload layer on stop. No host bind mounts, Git credential
	// helpers, personal home, Docker socket, or gateway credentials enter it.
	args := []string{"sandbox", "create", "--name", t.OpenShell.Name, "--from", c.Image, "--policy", c.Policy}
	if t.OpenShell.Provider != "" {
		args = append(args, "--provider", t.OpenShell.Provider)
	}
	args = append(args, "--provider", c.Providers[t.Session.Agent], "--no-auto-providers", "--approval-mode", "manual", "--detach", "--no-tty", "--label", "paddock.task="+t.ID, "--output", "json")
	if c.Memory != "" {
		args = append(args, "--memory", c.Memory)
	}
	if c.CPU != "" {
		args = append(args, "--cpu", c.CPU)
	}
	args = append(args, "--", "/bin/sleep", "infinity")
	created, e := c.call(ctx, args...)
	if e != nil {
		return e
	}
	var state sandboxState
	if e = json.Unmarshal(created, &state); e != nil || state.ID == "" {
		return fmt.Errorf("sandbox creation returned no identity")
	}
	t.OpenShell.ID = state.ID
	if e = s.Store.update(t.ID, func(v *Task) error { v.OpenShell = t.OpenShell; return nil }); e != nil {
		return e
	}
	// Enable the sandbox-local advisor before the coding agent starts. Denied
	// requests can then become narrow proposals and wait for the host operator
	// instead of being mistaken for a failed experiment.
	if _, e = c.call(ctx, "settings", "set", t.OpenShell.Name, "--key", "agent_policy_proposals_enabled", "--value", "true"); e != nil {
		return fmt.Errorf("enable OpenShell policy advisor: %w", e)
	}
	// Use a private temporary Git database to bundle exactly the selected commit
	// and its ancestry, including local unpushed bases; copy no Git configuration.
	seed := filepath.Join(dir, "seed.git")
	bundle := filepath.Join(dir, "seed.bundle")
	if _, e = git(ctx, dir, "init", "--bare", seed); e != nil {
		return e
	}
	if _, e = git(ctx, seed, "-c", "core.hooksPath="+os.DevNull, "fetch", "--no-tags", t.Repo, t.BaseCommit+":refs/heads/seed"); e != nil {
		return e
	}
	if _, e = git(ctx, seed, "bundle", "create", bundle, "refs/heads/seed"); e != nil {
		return e
	}
	if _, e = c.call(ctx, "sandbox", "upload", t.OpenShell.Name, bundle, "/sandbox/seed.bundle"); e != nil {
		return e
	}
	bootstrap := *t
	bootstrap.Worktree = "/sandbox"
	script := `set -eu
umask 077
git -c core.hooksPath=/dev/null clone --bare /sandbox/seed.bundle /sandbox/repository.git
if [ -n "$1" ]; then
  git --git-dir=/sandbox/repository.git remote set-url origin "$1"
  git --git-dir=/sandbox/repository.git config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
  gh auth setup-git
else
  git --git-dir=/sandbox/repository.git remote remove origin
fi
git --git-dir=/sandbox/repository.git config user.name "$3"
git --git-dir=/sandbox/repository.git config user.email "$4"
git --git-dir=/sandbox/repository.git -c core.hooksPath=/dev/null worktree add -b "$2" /sandbox/worktree refs/heads/seed
rm /sandbox/seed.bundle
mkdir -p /home/sandbox/.codex /home/sandbox/.claude
chmod 700 /home/sandbox/.codex /home/sandbox/.claude
`
	remoteURL := ""
	if t.OpenShell.Provider != "" {
		remoteURL = "https://github.com/" + t.OpenShell.Repository + ".git"
	}
	if _, e = sandboxOutput(ctx, bootstrap, "/bin/sh", "-c", script, "paddock", remoteURL, t.Branch, t.OpenShell.GitAuthorName, t.OpenShell.GitAuthorEmail); e != nil {
		return e
	}
	if _, e = sandboxOutput(ctx, *t, t.Session.AgentPath, "--version"); e != nil {
		return fmt.Errorf("sandbox image must provide %s: %w", t.Session.AgentPath, e)
	}
	if e = installHostShellShortcut(ctx, *t); e != nil {
		return fmt.Errorf("install host shell shortcut: %w", e)
	}
	if e = installInvestigationGuard(ctx, *t, dir); e != nil {
		return e
	}
	t.OpenShell.ReadOnlyGuard = t.Provenance.Profile == "investigation"
	if t.OpenShell.ReadOnlyGuard {
		t.OpenShell.ReadOnlyGuardVersion = investigationGuardVersion
	}
	t.OpenShell.Ready = true
	return nil
}

// A separate loop in the existing singleton keeps renewal independent of slow
// PR polling. Expiry is enforced by OpenShell if Paddock is offline.
func (s Service) RefreshSandboxCredentials(ctx context.Context) error {
	tasks, e := s.Store.List("")
	if e != nil {
		return e
	}
	var errs []error
	for _, t := range tasks {
		if t.RuntimeName() != "openshell" || t.OpenShell == nil || !t.OpenShell.Ready || t.OpenShell.Provider == "" {
			continue
		}
		if time.Until(t.OpenShell.CredentialExpiresAt) > 10*time.Minute {
			continue
		}
		state, e := inspectTaskSandbox(ctx, t)
		if e == nil && state.Phase != "Ready" {
			continue
		}
		if e != nil {
			errs = append(errs, e)
			continue
		}
		updateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		e = s.updateSandboxCredential(updateCtx, &t, false)
		cancel()
		_ = s.Store.update(t.ID, func(v *Task) error {
			if e != nil {
				v.OpenShell.CredentialError = e.Error()
			} else {
				v.OpenShell.CredentialExpiresAt = t.OpenShell.CredentialExpiresAt
				v.OpenShell.CredentialError = ""
			}
			return nil
		})
		if e != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, e))
		}
	}
	return errors.Join(errs...)
}

type sandboxState struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Phase  string            `json:"phase"`
	Labels map[string]string `json:"labels"`
}

func inspectTaskSandbox(ctx context.Context, t Task) (sandboxState, error) {
	var state sandboxState
	if t.OpenShell == nil {
		return state, fmt.Errorf("missing sandbox identity")
	}
	b, e := t.OpenShell.Config.call(ctx, "sandbox", "get", t.OpenShell.Name, "--output", "json")
	if e != nil {
		return state, e
	}
	if e = json.Unmarshal(b, &state); e != nil {
		return state, e
	}
	if (t.OpenShell.ID != "" && state.ID != t.OpenShell.ID) || state.Name != t.OpenShell.Name || state.Labels["paddock.task"] != t.ID {
		return state, fmt.Errorf("sandbox ownership does not match task")
	}
	return state, nil
}
func (s Service) startTaskSandbox(ctx context.Context, t *Task) error {
	proxy, proxyErr := sandboxAgentProxy(s.Store.Home, t.OpenShell.Config.Binary, t.Session.Agent)
	if proxyErr != nil {
		return proxyErr
	}
	t.OpenShell.AgentProxy = proxy
	state, e := inspectTaskSandbox(ctx, *t)
	if e != nil {
		return e
	}
	if !t.OpenShell.Ready {
		return fmt.Errorf("sandbox provisioning did not finish; inspect task before recovery")
	}
	if state.Phase == "Stopped" || state.Phase == "Completed" {
		if _, e = t.OpenShell.Config.call(ctx, "sandbox", "start", t.OpenShell.Name); e != nil {
			return e
		}
	} else if state.Phase != "Ready" {
		return fmt.Errorf("sandbox is %s; refusing to guess recovery", state.Phase)
	}
	// Refuse duplicate agents after a lost host supervisor/transport. The lock
	// lives inside the sandbox and is held by the native process's launcher.
	probe := *t
	probe.Worktree = "/sandbox"
	if _, e = sandboxOutput(ctx, probe, "python3", "-c", `import fcntl
f=open('/sandbox/agent.lock','a'); fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)`); e != nil {
		return fmt.Errorf("sandbox agent may still be active; refusing a second launch")
	}
	if t.OpenShell.Provider != "" {
		if e = s.updateSandboxCredential(ctx, t, false); e != nil {
			return e
		}
	}
	if e = s.Store.update(t.ID, func(v *Task) error {
		v.OpenShell.CredentialExpiresAt = t.OpenShell.CredentialExpiresAt
		v.OpenShell.CredentialError = ""
		v.OpenShell.AgentProxy = t.OpenShell.AgentProxy
		return nil
	}); e != nil {
		return e
	}
	return s.ensureRuntimePoller(ctx)
}
func stopCompletedSandbox(ctx context.Context, t Task) error {
	if _, e := inspectTaskSandbox(ctx, t); e != nil {
		return e
	}
	result, e := sandboxOutput(ctx, t, "cat", "/sandbox/agent-exit.json")
	if e != nil {
		return fmt.Errorf("remote agent exit is unconfirmed; sandbox retained running")
	}
	var marker struct {
		Generation int `json:"generation"`
		Code       int `json:"code"`
	}
	if json.Unmarshal([]byte(result), &marker) != nil || marker.Generation != t.Session.Generation {
		return fmt.Errorf("remote agent exit is unconfirmed; sandbox retained running")
	}
	if t.Session.Agent == "codex" {
		if _, e = sandboxOutput(ctx, t, "/usr/local/bin/paddock-codex-state", "idle", t.Session.ConversationID); e != nil {
			return e
		}
	}
	_, e = t.OpenShell.Config.call(ctx, "sandbox", "stop", t.OpenShell.Name)
	return e
}
func sandboxWorkspaceIdentity(ctx context.Context, t Task) error {
	if _, e := inspectTaskSandbox(ctx, t); e != nil {
		return e
	}
	root, e := taskGit(ctx, t, "rev-parse", "--show-toplevel")
	if e != nil {
		return e
	}
	common, e := taskGit(ctx, t, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if e != nil {
		return e
	}
	branch, e := taskGit(ctx, t, "symbolic-ref", "--quiet", "HEAD")
	if e != nil {
		return e
	}
	if root != t.Worktree || common != "/sandbox/repository.git" || branch != "refs/heads/"+t.Branch {
		return fmt.Errorf("sandbox checkout no longer matches task")
	}
	return nil
}
func restoreSandboxWorktree(ctx context.Context, t Task) (bool, error) {
	// Inspect absence explicitly; any other error must not trigger recreation.
	probe := t
	probe.Worktree = "/sandbox"
	state, e := sandboxOutput(ctx, probe, "python3", "-c", `import os;print('present' if os.path.lexists('/sandbox/worktree') else 'absent')`)
	if e != nil {
		return false, e
	}
	if state == "present" {
		return false, sandboxWorkspaceIdentity(ctx, t)
	}
	if state != "absent" {
		return false, fmt.Errorf("cannot inspect sandbox workspace")
	}
	head, e := sandboxOutput(ctx, probe, "git", "--git-dir=/sandbox/repository.git", "rev-parse", "--verify", "refs/heads/"+t.Branch+"^{commit}")
	if e != nil {
		return false, e
	}
	if t.WorkspaceRemovedAt != "" && t.WorkspaceCommit != "" && head != t.WorkspaceCommit {
		return false, fmt.Errorf("sandbox branch moved since GC")
	}
	// Without a GC checkpoint a manual deletion can have stale registrations;
	// leave it for explicit recovery, never force or prune another worktree.
	_, e = sandboxOutput(ctx, probe, "git", "--git-dir=/sandbox/repository.git", "-c", "core.hooksPath=/dev/null", "worktree", "add", "--", t.Worktree, t.Branch)
	return e == nil, e
}

func (s Service) ensureRuntimePoller(ctx context.Context) error {
	status, err := s.StartGitHubPoller(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	if status.RuntimeVersion < githubPollerVersion {
		return fmt.Errorf("the running poller predates the current runtime; restart it with paddock github stop then paddock github start")
	}
	return nil
}

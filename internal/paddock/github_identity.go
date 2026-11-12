package paddock

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A binding pins a public principal, not its key or rotating installation token.
// Rotating the key is safe; replacing the principal must not redirect queued posts.
type GitHubIdentityBinding struct {
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	Login          string `json:"login"`
	AppID          int64  `json:"app_id,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
}

func bindGitHubIdentity(name string, i GitHubIdentity) GitHubIdentityBinding {
	b := GitHubIdentityBinding{Name: name, Mode: i.Mode, Login: i.Login}
	if i.App != nil {
		b.AppID = i.App.AppID
		b.InstallationID = i.App.InstallationID
	}
	return b
}
func (s GitHubState) ResolveGitHubIdentity(repo string) (GitHubIdentityBinding, GitHubIdentity, error) {
	repo = strings.ToLower(repo)
	if !githubRepoName.MatchString(repo) {
		return GitHubIdentityBinding{}, GitHubIdentity{}, fmt.Errorf("invalid repository")
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, pattern := range []string{repo, owner + "/*", "*"} {
		if name, ok := s.Routes[pattern]; ok {
			i, exists := s.Identities[name]
			if !exists {
				return GitHubIdentityBinding{}, i, fmt.Errorf("route %s refers to missing identity %s", pattern, name)
			}
			if i.Disabled {
				return GitHubIdentityBinding{}, i, fmt.Errorf("identity %q is disabled", name)
			}
			return bindGitHubIdentity(name, i), i, nil
		}
	}
	return GitHubIdentityBinding{}, GitHubIdentity{}, fmt.Errorf("no GitHub identity is mapped to %s; use paddock github route", repo)
}
func (s GitHubState) boundIdentity(b GitHubIdentityBinding) (GitHubIdentity, error) {
	i, ok := s.Identities[b.Name]
	if !ok || i.Disabled || bindGitHubIdentity(b.Name, i) != b {
		return GitHubIdentity{}, fmt.Errorf("queued reply identity %q was removed or changed; restore that principal before sending", b.Name)
	}
	return i, nil
}
func (s *Store) RouteGitHubIdentity(pattern, name string) error {
	caller, err := LocalCaller()
	if err != nil {
		return err
	}
	pattern = strings.ToLower(pattern)
	valid := pattern == "*" || githubRepoName.MatchString(pattern)
	if owner, ok := strings.CutSuffix(pattern, "/*"); ok {
		valid = githubLogin.MatchString(owner) && !strings.Contains(owner, "[bot]")
	}
	if !valid {
		return fmt.Errorf("route must be owner/repository, owner/*, or *")
	}
	return s.UpdateGitHub(func(v *GitHubState) error {
		identity, ok := v.Identities[name]
		if !ok {
			return fmt.Errorf("unknown identity %q", name)
		}
		if identity.Owner != "" && identity.Owner != caller {
			return fmt.Errorf("caller is not the owner of identity %q", name)
		}
		if previousName, exists := v.Routes[pattern]; exists {
			previous := v.Identities[previousName]
			if previous.Owner != "" && previous.Owner != caller {
				return fmt.Errorf("caller is not the owner of repository route %q", pattern)
			}
		}
		v.Routes[pattern] = name
		return nil
	})
}
func (g GitHubCoordinator) remoteFor(state GitHubState, repo string, pinned *GitHubIdentityBinding) (GitHubRemote, GitHubIdentityBinding, error) {
	var binding GitHubIdentityBinding
	var identity GitHubIdentity
	var err error
	if pinned != nil && pinned.Name != "" {
		binding = *pinned
		identity, err = state.boundIdentity(binding)
	} else {
		binding, identity, err = state.ResolveGitHubIdentity(repo)
	}
	if err != nil {
		return nil, binding, err
	}
	if g.Remote != nil {
		return g.Remote, binding, nil
	} // deterministic integration-test transport
	data, _ := json.Marshal(identity)
	key := string(data)
	if api, ok := g.remotes[key]; ok {
		return api, binding, nil
	}
	api := NewGitHubAPI(identity)
	if g.remotes != nil {
		g.remotes[key] = api
	}
	return api, binding, nil
}
func NewGitHubCoordinator(service Service) GitHubCoordinator {
	return GitHubCoordinator{Service: service, remotes: map[string]GitHubRemote{}}
}

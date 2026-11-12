package paddock

import (
	"fmt"
	"os/user"
	"regexp"
	"strings"
)

var identityName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

// LocalCaller comes from the operating system, never from USER or an
// integration-supplied requester field.
func LocalCaller() (string, error) {
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return "", fmt.Errorf("cannot identify local caller")
	}
	return u.Uid + ":" + u.Username, nil
}

func normalizedProfile(profile string) string {
	if profile == "" {
		return "implementation"
	}
	return profile
}

func validProfile(profile string) bool {
	return profile == "investigation" || profile == "implementation"
}

func profileCapabilities(profile string) []string {
	return executionCapabilities(profile, true)
}

func executionCapabilities(profile string, githubApp bool) []string {
	if profile == "investigation" {
		capabilities := []string{"repository.read", "analysis"}
		if githubApp {
			capabilities = append(capabilities, "github.read")
		}
		return capabilities
	}
	capabilities := []string{"repository.read", "repository.write"}
	if githubApp {
		capabilities = append(capabilities, "github.read", "github.write", "pull_request.create")
	}
	return capabilities
}

func (s *Store) CreateIdentity(name, owner string, profiles []string) error {
	if !identityName.MatchString(name) || owner == "" {
		return fmt.Errorf("invalid identity name or owner")
	}
	if len(profiles) == 0 {
		profiles = []string{"investigation", "implementation"}
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		if !validProfile(p) || seen[p] {
			return fmt.Errorf("invalid or repeated execution profile %q", p)
		}
		seen[p] = true
	}
	return s.UpdateGitHub(func(v *GitHubState) error {
		if _, ok := v.Identities[name]; ok {
			return fmt.Errorf("identity %q already exists", name)
		}
		v.Identities[name] = GitHubIdentity{Owner: owner, CreatedAt: now(), Profiles: profiles}
		return nil
	})
}

func (s GitHubState) IdentityForCaller(name, profile, caller string) (GitHubIdentity, error) {
	i, ok := s.Identities[name]
	if !ok || i.Disabled {
		return i, fmt.Errorf("identity %q is unavailable", name)
	}
	if i.Owner != "" && i.Owner != caller {
		return i, fmt.Errorf("caller is not the owner of identity %q", name)
	}
	profile = normalizedProfile(profile)
	if !validProfile(profile) {
		return i, fmt.Errorf("unknown execution profile %q", profile)
	}
	if len(i.Profiles) != 0 {
		for _, allowed := range i.Profiles {
			if allowed == profile {
				return i, nil
			}
		}
		return i, fmt.Errorf("identity %q does not authorize %s", name, profile)
	}
	return i, nil // pre-existing credential profiles retain their former permissions
}

func (s Service) resolveProvenance(o RunOptions) (TaskProvenance, error) {
	caller, err := LocalCaller()
	if err != nil {
		return TaskProvenance{}, err
	}
	profile := normalizedProfile(o.Profile)
	if !validProfile(profile) {
		return TaskProvenance{}, fmt.Errorf("unknown execution profile %q", profile)
	}
	if len(o.Requester) > 256 || len(o.RequesterSource) > 128 || len(o.ExternalID) > 256 {
		return TaskProvenance{}, fmt.Errorf("provenance field is too long")
	}
	if len(o.Context) > 32 {
		return TaskProvenance{}, fmt.Errorf("too many context fields")
	}
	for k, v := range o.Context {
		if strings.TrimSpace(k) == "" || len(k) > 128 || len(v) > 4096 {
			return TaskProvenance{}, fmt.Errorf("invalid context field")
		}
	}
	if o.Identity != "" {
		state, err := s.Store.GitHubState()
		if err != nil {
			return TaskProvenance{}, err
		}
		if _, err = state.IdentityForCaller(o.Identity, profile, caller); err != nil {
			return TaskProvenance{}, err
		}
	}
	return TaskProvenance{Caller: caller, Requester: o.Requester, RequesterSource: o.RequesterSource, ExternalID: o.ExternalID, Context: o.Context, Identity: o.Identity, Profile: profile}, nil
}

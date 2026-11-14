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

func executionCapabilities(readOnly, githubApp bool) []string {
	capabilities := []string{"repository.read"}
	if readOnly {
		capabilities = append(capabilities, "analysis")
	} else {
		capabilities = append(capabilities, "repository.write")
	}
	if githubApp {
		capabilities = append(capabilities, "github.read")
		if !readOnly {
			capabilities = append(capabilities, "github.write", "pull_request.create")
		}
	}
	return capabilities
}

func (s *Store) CreateIdentity(name, owner, launcher string) error {
	if !identityName.MatchString(name) || owner == "" {
		return fmt.Errorf("invalid identity name or owner")
	}
	if _, err := s.LauncherConfig(launcher); err != nil {
		return err
	}
	return s.UpdateGitHub(func(v *GitHubState) error {
		if _, ok := v.Identities[name]; ok {
			return fmt.Errorf("identity %q already exists", name)
		}
		v.Identities[name] = GitHubIdentity{Owner: owner, CreatedAt: now(), Launcher: launcher}
		return nil
	})
}

func (s *Store) SetIdentityLauncher(name, caller, launcher string) error {
	if _, err := s.LauncherConfig(launcher); err != nil {
		return err
	}
	return s.UpdateGitHub(func(v *GitHubState) error {
		identity, err := v.IdentityForCaller(name, caller)
		if err != nil {
			return err
		}
		identity.Launcher = launcher
		v.Identities[name] = identity
		return nil
	})
}

func (s GitHubState) IdentityForCaller(name, caller string) (GitHubIdentity, error) {
	i, ok := s.Identities[name]
	if !ok || i.Disabled {
		return i, fmt.Errorf("identity %q is unavailable", name)
	}
	if i.Owner != "" && i.Owner != caller {
		return i, fmt.Errorf("caller is not the owner of identity %q", name)
	}
	return i, nil
}

func (s Service) resolveProvenance(o RunOptions) (TaskProvenance, error) {
	caller, err := LocalCaller()
	if err != nil {
		return TaskProvenance{}, err
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
		if _, err = state.IdentityForCaller(o.Identity, caller); err != nil {
			return TaskProvenance{}, err
		}
	}
	return TaskProvenance{Caller: caller, Requester: o.Requester, RequesterSource: o.RequesterSource, ExternalID: o.ExternalID, Context: o.Context, Identity: o.Identity, Launcher: o.Launcher}, nil
}

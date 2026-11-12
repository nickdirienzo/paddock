package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const identityHelp = `Agent identities:
  paddock identity create NAME [--profiles investigation,implementation]
  paddock identity list [--json]
  paddock identity show NAME [--json]
  paddock identity verify NAME [--repo owner/repo] [--json]
  paddock identity disable NAME
  paddock identity enable NAME
  paddock identity github setup NAME [--owner GITHUB_ORG] [--app-name NAME] [--visibility owner|any]
  paddock identity github associate NAME --app-id ID --installation-id ID --private-key-file PATH
  paddock identity github rotate NAME --app-id ID --installation-id ID --private-key-file PATH
  paddock identity github repositories NAME [--json]
  paddock identity github detach NAME

The local operating-system caller owns a new identity. GitHub setup requires a
human to register and install an App; see docs/github.md. Keys remain in the
protected host credential directory, not task state or sandboxes.
`

func runIdentity(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, _ = fmt.Fprint(out, identityHelp)
		return nil
	}
	command := args[0]
	if command == "github" {
		return runIdentityGitHub(args[1:], out)
	}
	allowed, ok := map[string]string{"create": "profiles", "list": "json", "show": "json", "verify": "repo json", "disable": "", "enable": ""}[command]
	if !ok {
		return fmt.Errorf("unknown identity command; use paddock identity --help")
	}
	flags, pos, err := parse(args[1:], allowed)
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		_, _ = fmt.Fprint(out, identityHelp)
		return nil
	}
	expected := 1
	if command == "list" {
		expected = 0
	}
	if len(pos) != expected {
		return fmt.Errorf("%s expects %d identity names", command, expected)
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	caller, err := paddock.LocalCaller()
	if err != nil {
		return err
	}
	if command == "create" {
		profiles := []string{}
		if flags["profiles"] != "" {
			profiles = strings.Split(flags["profiles"], ",")
		}
		if err = store.CreateIdentity(pos[0], caller, profiles); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "Created identity", pos[0])
		return nil
	}
	state, err := store.GitHubState()
	if err != nil {
		return err
	}
	if command == "list" {
		own := map[string]paddock.GitHubIdentity{}
		for name, identity := range state.Identities {
			if identity.Owner == "" || identity.Owner == caller {
				own[name] = identity
			}
		}
		return emit(out, own)
	}
	identity, exists := state.Identities[pos[0]]
	if !exists || (identity.Owner != "" && identity.Owner != caller) {
		return fmt.Errorf("identity %q is unavailable", pos[0])
	}
	switch command {
	case "show":
		if flags["json"] != "" {
			return emit(out, identity)
		}
		_, _ = fmt.Fprintf(out, "Identity: %s\nOwner: %s\nProfiles: %s\nDisabled: %t\nGitHub mode: %s\nGitHub actor: %s\n", pos[0], identity.Owner, strings.Join(identity.Profiles, ","), identity.Disabled, identity.Mode, identity.Login)
		if identity.App != nil {
			_, _ = fmt.Fprintf(out, "App ID: %d\nInstallation ID: %d\nKey file: %s\n", identity.App.AppID, identity.App.InstallationID, identity.App.KeyFile)
		}
		return nil
	case "verify":
		if identity.Disabled || identity.Mode != "app" {
			return fmt.Errorf("identity %q has no active GitHub App", pos[0])
		}
		verified, e := paddock.NewGitHubAPI(identity).AuthenticatedIdentity(context.Background())
		if e != nil {
			return e
		}
		botID, e := paddock.NewGitHubAPI(verified).BotUserID(context.Background(), verified.Login)
		if e != nil {
			return e
		}
		verifiedProfiles := []string{}
		if flags["repo"] != "" {
			api := paddock.NewGitHubAPI(verified)
			if e = api.VerifyRepository(context.Background(), flags["repo"]); e != nil {
				return e
			}
			profiles := identity.Profiles
			if len(profiles) == 0 {
				profiles = []string{"investigation", "implementation"}
			}
			for _, profile := range profiles {
				if e = api.VerifyExecutionProfile(context.Background(), flags["repo"], profile); e != nil {
					return fmt.Errorf("verify %s profile: %w", profile, e)
				}
				verifiedProfiles = append(verifiedProfiles, profile)
			}
		}
		if flags["json"] != "" {
			return emit(out, map[string]any{"name": pos[0], "github_actor": verified.Login, "bot_user_id": botID, "app_id": verified.App.AppID, "installation_id": verified.App.InstallationID, "repository": flags["repo"], "verified_profiles": verifiedProfiles, "verified": true})
		}
		_, _ = fmt.Fprintln(out, "Verified", pos[0], "as", verified.Login)
		return nil
	case "disable", "enable":
		if err = store.UpdateGitHub(func(v *paddock.GitHubState) error {
			current, ok := v.Identities[pos[0]]
			if !ok || (current.Owner != "" && current.Owner != caller) {
				return fmt.Errorf("identity %q is unavailable", pos[0])
			}
			current.Disabled = command == "disable"
			v.Identities[pos[0]] = current
			return nil
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, command+"d", pos[0])
		return nil
	}
	return nil
}

func runIdentityGitHub(args []string, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("use paddock identity github associate|rotate|detach NAME")
	}
	command, name := args[0], args[1]
	if command == "setup" {
		return runGitHubManifestSetup(name, args[2:], out)
	}
	if command == "repositories" {
		_, pos, err := parse(args[2:], "json")
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return fmt.Errorf("repositories takes no extra arguments")
		}
		home, err := paddock.StateHome()
		if err != nil {
			return err
		}
		store, err := paddock.OpenStore(home)
		if err != nil {
			return err
		}
		state, err := store.GitHubState()
		_ = store.Close()
		if err != nil {
			return err
		}
		caller, err := paddock.LocalCaller()
		if err != nil {
			return err
		}
		identity, ok := state.Identities[name]
		if !ok || identity.Disabled || identity.Mode != "app" || (identity.Owner != "" && identity.Owner != caller) {
			return fmt.Errorf("identity %q has no active App for this caller", name)
		}
		repos, err := paddock.NewGitHubAPI(identity).InstallationRepositories(context.Background())
		if err != nil {
			return err
		}
		return emit(out, repos)
	}
	if command == "associate" || command == "rotate" {
		home, err := paddock.StateHome()
		if err != nil {
			return err
		}
		store, err := paddock.OpenStore(home)
		if err != nil {
			return err
		}
		state, err := store.GitHubState()
		_ = store.Close()
		if err != nil {
			return err
		}
		caller, err := paddock.LocalCaller()
		if err != nil {
			return err
		}
		identity, ok := state.Identities[name]
		if !ok || (identity.Owner != "" && identity.Owner != caller) {
			return fmt.Errorf("create and own identity %q first", name)
		}
		if command == "rotate" && identity.Mode != "app" {
			return fmt.Errorf("identity %q has no App key to rotate", name)
		}
		if command == "rotate" {
			authArgs, err := rotationAuthArgs(name, identity, args[2:])
			if err != nil {
				return err
			}
			return runGitHub(authArgs, out)
		}
		return runGitHub(append([]string{"auth", "--name", name, "--mode", "app"}, args[2:]...), out)
	}
	if command != "detach" || len(args) != 2 {
		return fmt.Errorf("unknown identity GitHub command")
	}
	home, err := paddock.StateHome()
	if err != nil {
		return err
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		return err
	}
	defer store.Close()
	caller, err := paddock.LocalCaller()
	if err != nil {
		return err
	}
	var oldApp *paddock.GitHubAppConfig
	err = store.UpdateGitHub(func(v *paddock.GitHubState) error {
		identity, ok := v.Identities[name]
		if !ok || (identity.Owner != "" && identity.Owner != caller) {
			return fmt.Errorf("identity %q is unavailable", name)
		}
		oldApp = identity.App
		identity.Mode, identity.Login, identity.App = "", "", nil
		v.Identities[name] = identity
		return nil
	})
	if err != nil {
		return err
	}
	if oldApp != nil && oldApp.KeyFile != "" {
		if err := paddock.RemoveGitHubAppKeyForIdentity(oldApp.AppID, name, oldApp.KeyFile); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(out, "Detached GitHub App from", name)
	return nil
}

func rotationAuthArgs(name string, identity paddock.GitHubIdentity, args []string) ([]string, error) {
	if identity.Mode != "app" || identity.App == nil {
		return nil, fmt.Errorf("identity %q has no App key to rotate", name)
	}
	flags, pos, err := parse(args, "app-id installation-id private-key-file login")
	if err != nil {
		return nil, err
	}
	if len(pos) != 0 {
		return nil, fmt.Errorf("rotate takes no extra arguments")
	}
	for _, field := range []struct {
		flag string
		want int64
	}{{"app-id", identity.App.AppID}, {"installation-id", identity.App.InstallationID}} {
		if raw := flags[field.flag]; raw != "" {
			value, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || value != field.want {
				return nil, fmt.Errorf("rotation must keep the existing %s", field.flag)
			}
		}
	}
	if login := flags["login"]; login != "" && !strings.EqualFold(login, identity.Login) {
		return nil, fmt.Errorf("rotation must keep the existing GitHub actor")
	}
	if flags["private-key-file"] == "" && os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE") == "" && os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY") == "" {
		return nil, fmt.Errorf("rotation requires a new private key file or environment PEM")
	}
	result := []string{"auth", "--name", name, "--mode", "app", "--app-id", strconv.FormatInt(identity.App.AppID, 10), "--installation-id", strconv.FormatInt(identity.App.InstallationID, 10), "--login", identity.Login}
	if file := flags["private-key-file"]; file != "" {
		result = append(result, "--private-key-file", file)
	}
	return result, nil
}

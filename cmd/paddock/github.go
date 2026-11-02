package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const githubHelp = `GitHub coordination:
  paddock github auth --mode gh --login <developer>
  paddock github auth --mode app --login <team-bot[bot]> --token-env <VARIABLE>
  paddock github watch <PR-URL> --task <id> --allow <engineer,engineer>
  paddock github unwatch <PR-URL>
  paddock github poll [--interval 30s] [--json]
  paddock github status [--json]
  paddock github reply <event-id> "reply text"

Only explicitly watched PRs and authorized authors can create work.
App mode reads an installation token from the named environment variable.
No token is persisted. Without --interval, poll runs once and exits.
`

func runGitHub(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, githubHelp)
		return nil
	}
	command := args[0]
	allowed, ok := map[string]string{"auth": "mode login token-env", "watch": "task allow", "unwatch": "", "poll": "interval json", "status": "json", "reply": ""}[command]
	if !ok {
		return fmt.Errorf("unknown GitHub command; use paddock github --help")
	}
	flags, pos, err := parse(args[1:], allowed)
	if err != nil {
		return err
	}
	if flags["help"] != "" {
		fmt.Fprint(out, githubHelp)
		return nil
	}
	count := map[string]int{"auth": 0, "watch": 1, "unwatch": 1, "poll": 0, "status": 0, "reply": 2}[command]
	if len(pos) != count {
		return fmt.Errorf("%s expects %d arguments; see paddock github --help", command, count)
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
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	state, err := store.GitHubState()
	if err != nil {
		return err
	}
	api := paddock.GitHubAPI{Identity: state.Identity}
	coordinator := paddock.GitHubCoordinator{Service: paddock.Service{Store: store, Executable: exe}, Remote: api}
	switch command {
	case "auth":
		identity := paddock.GitHubIdentity{Mode: flags["mode"], Login: flags["login"], TokenEnv: flags["token-env"]}
		if identity.Mode == "gh" && identity.TokenEnv != "" {
			return fmt.Errorf("--token-env is only supported with --mode app")
		}
		if err = (paddock.GitHubAPI{Identity: identity}).ValidateIdentity(ctx); err != nil {
			return err
		}
		if err = store.UpdateGitHub(func(v *paddock.GitHubState) error { v.Identity = identity; return nil }); err != nil {
			return err
		}
		fmt.Fprintln(out, "GitHub identity:", identity.Login, "("+identity.Mode+")")
	case "watch":
		if flags["task"] == "" || flags["allow"] == "" {
			return fmt.Errorf("watch requires --task and --allow")
		}
		if err = coordinator.Watch(ctx, pos[0], flags["task"], strings.Split(flags["allow"], ",")); err != nil {
			return err
		}
		fmt.Fprintln(out, "Watching", pos[0], "for task", flags["task"])
	case "unwatch":
		repo, n, e := paddock.ParsePullRequest(pos[0])
		if e != nil {
			return e
		}
		key := fmt.Sprintf("%s#%d", repo, n)
		if err = store.UpdateGitHub(func(v *paddock.GitHubState) error { delete(v.Watches, key); return nil }); err != nil {
			return err
		}
		fmt.Fprintln(out, "Stopped watching", pos[0])
	case "status":
		return emit(out, state)
	case "reply":
		post, e := coordinator.Reply(ctx, pos[0], pos[1])
		if e != nil {
			return e
		}
		fmt.Fprintln(out, post.URL)
	case "poll":
		var interval time.Duration
		if flags["interval"] != "" {
			interval, err = time.ParseDuration(flags["interval"])
			if err != nil || interval < 5*time.Second {
				return fmt.Errorf("--interval must be at least 5s")
			}
		}
		for {
			events, e := coordinator.Poll(ctx)
			if e != nil {
				return e
			}
			if flags["json"] != "" {
				if err = emit(out, events); err != nil {
					return err
				}
			} else {
				for _, event := range events {
					fmt.Fprintf(out, "%s %s %s %s\n", event.ID, event.Plan.Action, event.Status, event.Comment.HTMLURL)
					if event.Error != "" {
						fmt.Fprintln(out, " ", event.Error)
					}
				}
				if len(events) == 0 {
					fmt.Fprintln(out, "No new comments.")
				}
			}
			if interval == 0 {
				return nil
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
	return nil
}

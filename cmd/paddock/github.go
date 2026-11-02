package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

const githubHelp = `GitHub coordination:
  paddock github auth --name dev --mode gh --login <developer>
  paddock github auth --name personal --mode app --app-id <id> --installation-id <id> --private-key-file <path>
  paddock github auth --mode app  # PADDOCK_GITHUB_* environment configuration
  paddock github installations --app-id <id> --private-key-file <path>
  paddock github route <owner/repo|owner/*|*> --identity <name>
  paddock github unroute <pattern>
  paddock github identities
  paddock github watch <PR-URL> --task <id> --allow <engineer,engineer>
  paddock github unwatch <PR-URL>
  paddock github poll [--interval 30s] [--json]
  paddock github status [--json]
  paddock github reply <event-id> "reply text" [--queue]

Only explicitly watched PRs and authorized authors can create work.
App mode signs JWTs with a private key and refreshes installation tokens in memory.
No token is persisted. Without --interval, poll runs once and exits.
`

func runGitHub(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, githubHelp)
		return nil
	}
	command := args[0]
	allowed, ok := map[string]string{"auth": "name mode login app-id installation-id private-key-file", "installations": "app-id private-key-file", "route": "identity", "unroute": "", "identities": "", "watch": "task allow", "unwatch": "", "poll": "interval json", "status": "json", "reply": "queue"}[command]
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
	count := map[string]int{"auth": 0, "installations": 0, "route": 1, "unroute": 1, "identities": 0, "watch": 1, "unwatch": 1, "poll": 0, "status": 0, "reply": 2}[command]
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
	coordinator := paddock.NewGitHubCoordinator(paddock.Service{Store: store, Executable: exe})
	switch command {
	case "installations":
		raw := flags["app-id"]
		if raw == "" {
			raw = os.Getenv("PADDOCK_GITHUB_APP_ID")
		}
		appID, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || appID < 1 {
			return fmt.Errorf("provide a positive --app-id or PADDOCK_GITHUB_APP_ID")
		}
		keyFile := flags["private-key-file"]
		if keyFile == "" {
			keyFile = os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE")
		}
		if keyFile != "" && os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY") != "" {
			return fmt.Errorf("choose a private key file or environment PEM, not both")
		}
		entries, e := paddock.DiscoverGitHubAppInstallations(ctx, paddock.GitHubAppConfig{AppID: appID, KeyFile: keyFile, KeyEnvironment: keyFile == ""})
		if e != nil {
			return e
		}
		return emit(out, entries)

	case "auth":
		identity := paddock.GitHubIdentity{Mode: flags["mode"], Login: flags["login"]}
		if identity.Mode == "app" {
			config := paddock.GitHubAppConfig{}
			values := []struct {
				flag, env string
				dest      *int64
			}{{"app-id", "PADDOCK_GITHUB_APP_ID", &config.AppID}, {"installation-id", "PADDOCK_GITHUB_INSTALLATION_ID", &config.InstallationID}}
			for _, value := range values {
				raw := flags[value.flag]
				if raw == "" {
					raw = os.Getenv(value.env)
				}
				n, e := strconv.ParseInt(raw, 10, 64)
				if e != nil || n < 1 {
					return fmt.Errorf("--%s or %s must be a positive integer", value.flag, value.env)
				}
				*value.dest = n
			}
			keyPath := flags["private-key-file"]
			if keyPath == "" {
				keyPath = os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE")
			}
			if keyPath != "" && os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY") != "" {
				return fmt.Errorf("choose a private key file or environment PEM, not both")
			}
			if keyPath != "" {
				// Validate credentials against GitHub before storing a protected local copy.
				keyPath, err = filepath.Abs(keyPath)
				if err != nil {
					return err
				}
				config.KeyFile = keyPath
			} else {
				config.KeyEnvironment = true
			}
			identity.App = &config
		} else if flags["app-id"] != "" || flags["installation-id"] != "" || flags["private-key-file"] != "" {
			return fmt.Errorf("App credential options require --mode app")
		}
		identity, err = paddock.NewGitHubAPI(identity).AuthenticatedIdentity(ctx)
		if err != nil {
			return err
		}
		if identity.App != nil && identity.App.KeyFile != "" {
			identity.App.KeyFile, err = paddock.ImportGitHubAppKey(identity.App.AppID, identity.App.KeyFile)
			if err != nil {
				return err
			}
		}
		name := flags["name"]
		if name == "" {
			name = "default"
		}
		if strings.ContainsAny(name, "/ *\\\t\n") || name == "" {
			return fmt.Errorf("identity name must be a simple local name")
		}
		if err = store.UpdateGitHub(func(v *paddock.GitHubState) error {
			v.Identities[name] = identity
			if flags["name"] == "" {
				v.Routes["*"] = name
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintln(out, "GitHub identity:", name, "->", identity.Login, "("+identity.Mode+")")
	case "route":
		if err = store.RouteGitHubIdentity(pos[0], flags["identity"]); err != nil {
			return err
		}
		fmt.Fprintln(out, pos[0], "->", flags["identity"])
	case "unroute":
		if err = store.UpdateGitHub(func(v *paddock.GitHubState) error { delete(v.Routes, strings.ToLower(pos[0])); return nil }); err != nil {
			return err
		}
		fmt.Fprintln(out, "Removed route", pos[0])
	case "identities":
		return emit(out, state.Identities)
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
		queue := flags["queue"] != ""
		if os.Getenv("PADDOCK_TASK_ID") != "" {
			event, ok := state.Events[pos[0]]
			if !ok {
				return fmt.Errorf("event not found")
			}
			watch, ok := state.Watches[event.WatchKey]
			if !ok {
				return fmt.Errorf("event no longer has an active watch")
			}
			binding, _, e := state.ResolveGitHubIdentity(watch.Repo)
			if e != nil {
				return e
			}
			queue = queue || binding.Mode == "app"
		}
		if queue {
			post, e := coordinator.EnqueueReply(pos[0], pos[1])
			if e != nil {
				return e
			}
			if post.Status == "posted" {
				fmt.Fprintln(out, post.URL)
			} else {
				fmt.Fprintln(out, "Reply", post.ID, "is", post.Status+"; run paddock github poll to publish queued replies.")
			}
			return nil
		}
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
				var receiptError *paddock.GitHubReceiptError
				if !errors.As(e, &receiptError) {
					return e
				}
				if interval != 0 {
					fmt.Fprintln(os.Stderr, "paddock:", e)
				}
			}
			if flags["json"] != "" {
				if err = emit(out, events); err != nil {
					return err
				}
			} else {
				for _, event := range events {
					fmt.Fprintf(out, "%s %s %s %s\n", event.ID, event.Plan.Action, event.Status, event.Comment.HTMLURL)
					if event.Receipt != "" {
						fmt.Fprintln(out, "  eyes:", event.Receipt)
					}
					if event.Error != "" {
						fmt.Fprintln(out, " ", event.Error)
					}
				}
				if len(events) == 0 {
					fmt.Fprintln(out, "No new comments.")
				}
			}
			if interval == 0 {
				return e
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

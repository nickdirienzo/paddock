package main

import (
	"context"
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
  paddock github routing <PR-URL> --mode direct|planner [--tasks id,id] [--repos alias,path] [--model MODEL]
  paddock github unwatch <PR-URL>
  paddock github start [--interval 30s]
  paddock github stop
  paddock github poll [--interval 30s] [--json]
  paddock github status [--json]
  paddock github reply <event-id> "reply text" [--queue]

Only explicitly watched PRs and authorized authors can create work.
Use --allow '*' to accept any GitHub user on that PR (bots remain excluded).
Quote '*' to prevent shell expansion. Named allowlists remain the default.
App mode signs JWTs with a private key and refreshes installation tokens in memory.
No token is persisted. Watch registration starts or reuses one background poller.
Without --interval, poll runs once and exits; interval polling runs in the foreground.
`

func runGitHub(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, githubHelp)
		return nil
	}
	command := args[0]
	allowed, ok := map[string]string{"auth": "name mode login app-id installation-id private-key-file", "installations": "app-id private-key-file", "route": "identity", "unroute": "", "identities": "", "watch": "task allow", "routing": "mode tasks repos model", "unwatch": "", "start": "interval", "stop": "", "poll": "interval json", "status": "json", "reply": "queue"}[command]
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
	count := map[string]int{"auth": 0, "installations": 0, "route": 1, "unroute": 1, "identities": 0, "watch": 1, "routing": 1, "unwatch": 1, "start": 0, "stop": 0, "poll": 0, "status": 0, "reply": 2}[command]
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
		status, e := coordinator.Service.StartGitHubPoller(ctx, 30*time.Second)
		if e != nil {
			return fmt.Errorf("watch registered, but poller startup failed: %w", e)
		}
		fmt.Fprintln(out, "Shared GitHub poller running, PID", status.PID)
	case "routing":
		if err = coordinator.ConfigureRouting(ctx, pos[0], flags["mode"], flags["model"], csv(flags["tasks"]), csv(flags["repos"])); err != nil {
			return err
		}
		fmt.Fprintln(out, "Routing", pos[0], "with", flags["mode"])
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
	case "start":
		interval := 30 * time.Second
		if flags["interval"] != "" {
			interval, err = time.ParseDuration(flags["interval"])
			if err != nil || interval < 5*time.Second {
				return fmt.Errorf("--interval must be at least 5s")
			}
		}
		status, e := coordinator.Service.StartGitHubPoller(ctx, interval)
		if e != nil {
			return e
		}
		fmt.Fprintln(out, "Shared GitHub poller running, PID", status.PID, "interval", status.Interval)
	case "stop":
		if err := store.StopGitHubPoller(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "Shared GitHub poller stopped; watches retained.")
	case "status":
		status, e := store.GitHubPollerStatus()
		if e != nil {
			return e
		}
		sessions := map[string]string{}
		for key, watch := range state.Watches {
			sessions[key] = coordinator.Service.WatchSessionState(ctx, watch.TaskID)
		}
		return emit(out, struct {
			paddock.GitHubState
			Poller   paddock.GitHubPollerStatus `json:"poller"`
			Sessions map[string]string          `json:"watch_sessions"`
		}{state, status, sessions})
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
				fmt.Fprintln(out, "Reply", post.ID, "is", post.Status+"; the shared GitHub poller publishes queued replies.")
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
		report := func(events []paddock.GitHubEvent, pollErr error) error {
			if pollErr != nil {
				fmt.Fprintln(os.Stderr, "paddock:", pollErr)
			}
			if flags["json"] != "" {
				return emit(out, events)
			}
			for _, event := range events {
				fmt.Fprintf(out, "%s %s %s %s\n", event.ID, event.Plan.Action, event.Status, event.Comment.HTMLURL)
				if event.Receipt != "" {
					fmt.Fprintln(out, "  eyes:", event.Receipt)
				}
				if event.Error != "" {
					fmt.Fprintln(out, " ", event.Error)
				}
			}
			if len(events) == 0 && interval == 0 {
				fmt.Fprintln(out, "No new comments.")
			}
			return nil
		}
		if interval != 0 {
			return coordinator.RunGitHubPoller(ctx, interval, report)
		}
		events, e := coordinator.Poll(ctx)
		if err := report(events, nil); err != nil {
			return err
		}
		return e
	}
	return nil
}

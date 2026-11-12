package paddock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const githubPollerVersion = 7 // v7 requires OpenShell-only routing and identity-aware task state.

// GitHubPollerStatus describes the shared process, not an individual task.
// Running is checked against its OS lock; a stale PID never implies liveness.
type GitHubPollerStatus struct {
	RuntimeVersion int    `json:"runtime_version,omitempty"`
	Running        bool   `json:"running"`
	Instance       string `json:"instance,omitempty"`
	PID            int    `json:"pid,omitempty"`
	Mode           string `json:"mode,omitempty"`
	Interval       string `json:"interval,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	LastPoll       string `json:"last_poll,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	Log            string `json:"log"`
}

func (s *Store) pollerFile(name string) string { return filepath.Join(s.Home, "github-poller"+name) }

func (s *Store) pollerLock(name string, shared bool) (*os.File, bool, error) {
	f, err := os.OpenFile(s.pollerFile(name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if shared {
		ok, err = tryPollerReadLock(f)
	} else {
		ok, err = tryLock(f)
	}
	if err != nil || !ok {
		f.Close()
		return nil, false, err
	}
	return f, true, nil
}
func releasePollerLock(f *os.File) { unlock(f); f.Close() }

func (s *Store) GitHubPollerStatus() (GitHubPollerStatus, error) {
	status := GitHubPollerStatus{Log: s.pollerFile(".log")}
	f, free, err := s.pollerLock("", true)
	if err != nil {
		return status, err
	}
	if free {
		defer releasePollerLock(f)
	}
	data, err := os.ReadFile(s.pollerFile(".json"))
	if err == nil {
		err = json.Unmarshal(data, &status)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	status.Running = !free
	return status, nil
}

func (s *Store) pollerControl(ctx context.Context) (func(), error) {
	for {
		f, ok, err := s.pollerLock("-control", false)
		if err != nil {
			return nil, err
		}
		if ok {
			return func() { releasePollerLock(f) }, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StartGitHubPoller is idempotent, including concurrent registrations. Its child
// owns a process-lifetime lease and has no terminal or task lifecycle dependency.
func (s Service) StartGitHubPoller(ctx context.Context, interval time.Duration) (GitHubPollerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := s.Store.pollerControl(ctx)
	if err != nil {
		return GitHubPollerStatus{}, err
	}
	defer release()
	status, err := s.Store.GitHubPollerStatus()
	if err != nil || status.Running {
		return status, err
	}
	if interval < 5*time.Second {
		return status, fmt.Errorf("poll interval must be at least 5s")
	}
	instance := newID()
	log, err := os.OpenFile(status.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return status, err
	}
	defer log.Close()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return status, err
	}
	defer stdin.Close()
	cmd := exec.Command(s.Executable, "github", "poll", "--interval", interval.String())
	cmd.Dir = s.Store.Home
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "PADDOCK_HOME", "PADDOCK_TASK_ID", "PADDOCK_PARENT_TASK_ID", "PADDOCK_POLLER_INSTANCE":
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "PADDOCK_HOME="+s.Store.Home, "PADDOCK_POLLER_INSTANCE="+instance)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, log, log
	detachPoller(cmd)
	if err = cmd.Start(); err != nil {
		return status, err
	}
	// Reap when this caller remains alive; the detached child survives caller exit.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		status, err = s.Store.GitHubPollerStatus()
		if err != nil {
			return status, err
		}
		if status.Running && status.Instance == instance {
			return status, nil
		}
		select {
		case err := <-done:
			return status, fmt.Errorf("poller exited during startup (%v); see %s", err, status.Log)
		case <-ctx.Done():
			// The instance-qualified request also cancels a child that starts late.
			_ = writeJSONAtomic(s.Store.pollerFile("-stop.json"), instance)
			return status, fmt.Errorf("poller startup: %w; see %s", ctx.Err(), status.Log)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Stop requests cancellation by instance ID, never by a potentially reused PID.
func (s *Store) StopGitHubPoller(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := s.pollerControl(ctx)
	if err != nil {
		return err
	}
	defer release()
	status, err := s.GitHubPollerStatus()
	if err != nil || !status.Running {
		return err
	}
	if status.Instance == "" {
		return fmt.Errorf("poller is starting; retry stop")
	}
	if err = writeJSONAtomic(s.pollerFile("-stop.json"), status.Instance); err != nil {
		return err
	}
	for {
		status, err = s.GitHubPollerStatus()
		if err != nil || !status.Running {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("poller stop: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RunGitHubPoller holds the singleton lease for both foreground and detached
// loops. Remote failures are recorded and retried; stopping cancels in-flight IO.
func (g GitHubCoordinator) RunGitHubPoller(ctx context.Context, interval time.Duration, report func([]GitHubEvent, error) error) error {
	if interval < 5*time.Second {
		return fmt.Errorf("poll interval must be at least 5s")
	}
	s := g.Service.Store
	// Status readers briefly hold shared locks. Allow them to finish before
	// treating a busy lease as another runner.
	var f *os.File
	var err error
	deadline := time.Now().Add(time.Second)
	for {
		var ok bool
		f, ok, err = s.pollerLock("", false)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("GitHub poller already running; use github status or github stop")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer releasePollerLock(f)
	instance := os.Getenv("PADDOCK_POLLER_INSTANCE")
	mode := "background"
	if instance == "" {
		mode = "foreground"
		instance = newID()
	}
	status := GitHubPollerStatus{RuntimeVersion: githubPollerVersion, Running: true, Instance: instance, PID: os.Getpid(), Mode: mode, Interval: interval.String(), StartedAt: now(), Log: s.pollerFile(".log")}
	if err = writeJSONAtomic(s.pollerFile(".json"), status); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			data, err := os.ReadFile(s.pollerFile("-stop.json"))
			var requested string
			if err == nil && json.Unmarshal(data, &requested) == nil && requested == instance {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	shelled := make(chan struct{})
	go func() { defer close(shelled); g.Service.RunShellRequests(ctx) }()
	defer func() { cancel(); <-shelled }()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			_ = g.Service.RefreshSandboxCredentials(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	accessed := make(chan struct{})
	go func() {
		defer close(accessed)
		ticker := time.NewTicker(accessPollInterval)
		defer ticker.Stop()
		lastError := ""
		for {
			err := g.Service.SyncRunningAccess(ctx)
			message := ""
			if err != nil {
				message = err.Error()
			}
			if message != "" && message != lastError {
				fmt.Fprintln(os.Stderr, "OpenShell access request sync:", message)
			}
			lastError = message
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancel(); <-stopped; <-renewed; <-accessed }()
	for ctx.Err() == nil {
		// Bound a pass so a stalled API cannot prevent later polls forever.
		pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Minute)
		events, pollErr := g.Poll(pollCtx)
		pollCancel()
		status.LastPoll, status.LastError = now(), ""
		if pollErr != nil {
			status.LastError = pollErr.Error()
		}
		if err = writeJSONAtomic(s.pollerFile(".json"), status); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if report != nil {
			if err = report(events, pollErr); err != nil {
				return err
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

// WatchSessionState checks the agent as well as its terminal. An idle terminal
// after an agent exits is not a delivery destination.
func (s Service) WatchSessionState(ctx context.Context, id string) string {
	task, err := s.Store.Get(id)
	if err != nil {
		return "missing"
	}
	if task.RuntimeName() != "openshell" && s.messageDelivery == nil {
		return "unsupported"
	}
	if !nativeMessageSupported(task) {
		return "unsupported"
	}
	if task.Status != "running" {
		if task.Session.ConversationID != "" && task.Status != "starting" && task.Status != "preparing" {
			_, observation := terminalFor(task.Session).Inspect(ctx, task)
			if observation.State == "missing" || observation.State == "exited" {
				return "resumable"
			}
		}
		return "offline"
	}
	task, observation, err := s.Refresh(ctx, task)
	if err != nil || observation.State == "unknown" {
		return "unknown"
	}
	if task.Status == "running" && observation.State == "running" {
		if task.Session.Agent == "claude" && task.RuntimeName() == "host" {
			if _, err := s.Store.hostClaudeInbox(task); err != nil {
				return "offline"
			}
		}
		return "online"
	}
	return "offline"
}

func (s Service) CanDeliver(ctx context.Context, id string) bool {
	state := s.WatchSessionState(ctx, id)
	return state == "online" || state == "resumable"
}

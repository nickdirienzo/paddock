package paddock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type GitHubRemote interface {
	ValidateIdentity(context.Context) error
	Pull(context.Context, string, int) (PullRequest, error)
	UserID(context.Context, string) (int64, error)
	Comments(context.Context, string, int) ([]GitHubComment, error)
	Reply(context.Context, string, int, GitHubComment, string) (GitHubComment, error)
}
type GitHubCoordinator struct {
	Service Service
	Remote  GitHubRemote
}

func (g GitHubCoordinator) Watch(ctx context.Context, raw, taskID string, authors []string) error {
	repo, n, err := ParsePullRequest(raw)
	if err != nil {
		return err
	}
	task, err := g.Service.Store.Get(taskID)
	if err != nil {
		return err
	}
	if len(authors) == 0 {
		return fmt.Errorf("--allow requires at least one engineer login")
	}
	if err = g.Remote.ValidateIdentity(ctx); err != nil {
		return err
	}
	pr, err := g.Remote.Pull(ctx, repo, n)
	if err != nil {
		return err
	}
	if pr.State != "open" || !strings.EqualFold(pr.Base.Repo.FullName, repo) || pr.Head.Ref != task.Branch {
		return fmt.Errorf("open PR head branch must match the owning task branch %s", task.Branch)
	}
	remote, err := git(ctx, task.Worktree, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimPrefix(remote, "https://github.com/")
	remote = strings.TrimPrefix(remote, "git@github.com:")
	remote = strings.TrimPrefix(remote, "ssh://git@github.com/")
	if !strings.EqualFold(remote, pr.Head.Repo.FullName) {
		return fmt.Errorf("PR head repository does not match the task's origin")
	}
	w := GitHubWatch{Repo: repo, Number: n, TaskID: task.ID, HeadRepo: pr.Head.Repo.FullName, HeadRef: pr.Head.Ref, Since: time.Now().UTC(), Authors: map[string]int64{}}
	for _, login := range authors {
		login = strings.TrimSpace(login)
		id, err := g.Remote.UserID(ctx, login)
		if err != nil {
			return err
		}
		w.Authors[login] = id
	}
	return g.Service.Store.UpdateGitHub(func(v *GitHubState) error {
		if old, ok := v.Watches[w.Key()]; ok {
			if old.TaskID != task.ID {
				return fmt.Errorf("PR is already owned by task %s", old.TaskID)
			}
			w.Since = old.Since
		}
		v.Watches[w.Key()] = w
		return nil
	})
}

// A separate OS lock serializes pollers across network calls. Task state locking
// remains short-lived, so agents can delegate or report results during polling.
func (g GitHubCoordinator) lockPoll() (func(), error) {
	f, err := os.OpenFile(filepath.Join(g.Service.Store.Home, ".github-poll.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		f.Close()
		if err == nil {
			err = fmt.Errorf("another GitHub poll or reply is active")
		}
		return nil, err
	}
	return func() { unlock(f); f.Close() }, nil
}

func (g GitHubCoordinator) Poll(ctx context.Context) ([]GitHubEvent, error) {
	release, err := g.lockPoll()
	if err != nil {
		return nil, err
	}
	defer release()
	if err = g.Remote.ValidateIdentity(ctx); err != nil {
		return nil, err
	}
	state, err := g.Service.Store.GitHubState()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(state.Watches))
	for key := range state.Watches {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	processed := []GitHubEvent{}
	for _, key := range keys {
		w := state.Watches[key]
		pr, err := g.Remote.Pull(ctx, w.Repo, w.Number)
		if err != nil {
			return processed, err
		}
		if pr.State != "open" {
			continue
		}
		if pr.Head.Ref != w.HeadRef || !strings.EqualFold(pr.Head.Repo.FullName, w.HeadRepo) || !strings.EqualFold(pr.Base.Repo.FullName, w.Repo) {
			return processed, fmt.Errorf("PR ownership changed for %s; update its watch explicitly", key)
		}
		comments, err := g.Remote.Comments(ctx, w.Repo, w.Number)
		if err != nil {
			return processed, err
		}
		sort.Slice(comments, func(i, j int) bool {
			if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
				return comments[i].ID < comments[j].ID
			}
			return comments[i].CreatedAt.Before(comments[j].CreatedAt)
		})
		for _, comment := range comments {
			if comment.CreatedAt.Before(w.Since) {
				continue
			}
			id := githubEventID(w, comment)
			if old, exists := state.Events[id]; exists {
				if old.Status == "claimed" {
					old.Status = "uncertain"
					old.Error = "previous poll ended during an action; inspect the agent before taking further action"
					if err = g.saveEvent(old); err != nil {
						return processed, err
					}
				}
				continue
			}
			plan := Plan{Action: "ignore", Kind: "discussion", Reason: "author is not authorized"}
			// Identity and authorization are enforced before the planner sees the text.
			self := comment.User.Type == "Bot" || (state.Identity.Mode == "app" && strings.EqualFold(comment.User.Login, state.Identity.Login)) || strings.Contains(comment.Body, "<!-- paddock:")
			if self {
				plan.Reason = "bot or Paddock-generated comment"
			} else if authorizedGitHub(w, comment) {
				plan = PlanRequest(Request{Body: comment.Body, Address: state.Identity.Login})
			}
			event := GitHubEvent{ID: id, WatchKey: key, TaskID: w.TaskID, Comment: comment, Plan: plan}
			claimed, err := g.Service.Store.claimGitHub(event)
			if err != nil {
				return processed, err
			}
			if !claimed {
				continue
			}
			event.Status = "done"
			if plan.Action != "ignore" {
				task, err := g.Service.Store.Get(w.TaskID)
				if err != nil {
					event.Status = "failed"
					event.Error = err.Error()
				} else {
					switch plan.Action {
					case "message":
						message := fmt.Sprintf("GitHub request from %s on %s\nReview context: %s:%d\n\n%s\n\nReport your result to the PR with: PADDOCK_HOME=%s %s github reply %s %s", comment.User.Login, comment.HTMLURL, comment.Path, comment.Line, plan.Text, quote(g.Service.Store.Home), quote(g.Service.Executable), id, quote("Describe the result and validation."))
						thread, e := g.Service.Message(ctx, task, message)
						if e != nil {
							event.Status = "uncertain"
							event.Error = e.Error()
						} else {
							event.Result = "Queued for agent session " + thread
						}
					case "delegate":
						child, e := g.Service.Run(ctx, RunOptions{Prompt: plan.Text + "\n\nRequested in " + comment.HTMLURL, Parent: task.ID, Agent: task.Session.Agent, Base: task.Branch})
						if e != nil {
							event.Status = "uncertain"
							event.Error = e.Error()
						} else {
							event.Result = "Delegated to task " + child.ID
						}
					case "reply":
						event.Result = plan.Text
						if plan.Text == "status" || plan.Text == "status?" {
							event.Result = fmt.Sprintf("Task `%s` is `%s` (%s).", task.ID, task.Status, task.Session.Agent)
						}
						if plan.Text == "help" {
							event.Result = "Use `paddock: message: ...`, `paddock: delegate: ...`, or `paddock: status`. Questions addressed to this identity go to the owning agent."
						}
					default:
						event.Status = "failed"
						event.Error = "unsupported planner action"
					}
				}
			}
			event.UpdatedAt = now()
			if err = g.saveEvent(event); err != nil {
				return processed, err
			}
			processed = append(processed, event)
			state.Events[id] = event
		}
	}
	// Resume result posting independently from agent delivery. Posting failures must
	// never cause a successful agent action to be repeated.
	current, err := g.Service.Store.GitHubState()
	if err != nil {
		return processed, err
	}
	for _, event := range current.Events {
		if event.Status == "done" && event.Plan.Action != "ignore" && event.Result != "" {
			if _, ok := current.Watches[event.WatchKey]; !ok {
				continue
			}
			if _, err = g.reply(ctx, event.ID, event.Result); err != nil {
				return processed, err
			}
		}
	}
	return processed, nil
}
func (g GitHubCoordinator) saveEvent(event GitHubEvent) error {
	event.UpdatedAt = now()
	return g.Service.Store.UpdateGitHub(func(v *GitHubState) error { v.Events[event.ID] = event; return nil })
}

func (g GitHubCoordinator) Reply(ctx context.Context, eventID, body string) (GitHubPost, error) {
	release, err := g.lockPoll()
	if err != nil {
		return GitHubPost{}, err
	}
	defer release()
	if err = g.Remote.ValidateIdentity(ctx); err != nil {
		return GitHubPost{}, err
	}
	return g.reply(ctx, eventID, body)
}
func (g GitHubCoordinator) reply(ctx context.Context, eventID, body string) (GitHubPost, error) {
	var post GitHubPost
	if strings.TrimSpace(body) == "" {
		return post, fmt.Errorf("reply cannot be empty")
	}
	state, err := g.Service.Store.GitHubState()
	if err != nil {
		return post, err
	}
	event, ok := state.Events[eventID]
	if !ok {
		return post, fmt.Errorf("event not found")
	}
	if task := os.Getenv("PADDOCK_TASK_ID"); task != "" && task != event.TaskID {
		return post, fmt.Errorf("this agent does not own the PR event")
	}
	w, ok := state.Watches[event.WatchKey]
	if !ok || w.TaskID != event.TaskID {
		return post, fmt.Errorf("event no longer has an active watch")
	}
	if !authorizedGitHub(w, event.Comment) {
		return post, fmt.Errorf("event author is no longer authorized")
	}
	id := githubID(eventID + "\x00" + body)
	if existing, exists := state.Posts[id]; exists {
		if existing.Status == "posted" {
			return existing, nil
		}
		// Reconcile a possibly successful POST, but never blindly issue it again.
		comments, err := g.Remote.Comments(ctx, w.Repo, w.Number)
		if err != nil {
			return existing, err
		}
		for _, c := range comments {
			if strings.EqualFold(c.User.Login, state.Identity.Login) && strings.Contains(c.Body, "<!-- paddock:"+id+" -->") {
				existing.Status = "posted"
				existing.URL = c.HTMLURL
				existing.Error = ""
				err = g.savePost(existing)
				return existing, err
			}
		}
		return existing, fmt.Errorf("reply %s has an uncertain outcome; inspect GitHub before posting another reply", id)
	}
	post = GitHubPost{ID: id, EventID: eventID, Body: body, Status: "posting"}
	if err = g.savePost(post); err != nil {
		return post, err
	}
	result, err := g.Remote.Reply(ctx, w.Repo, w.Number, event.Comment, body+"\n\n<!-- paddock:"+id+" -->")
	if err != nil {
		post.Status = "uncertain"
		post.Error = err.Error()
		if e := g.savePost(post); e != nil {
			return post, e
		}
		return post, err
	}
	post.Status = "posted"
	post.URL = result.HTMLURL
	return post, g.savePost(post)
}
func (g GitHubCoordinator) savePost(post GitHubPost) error {
	return g.Service.Store.UpdateGitHub(func(v *GitHubState) error { v.Posts[post.ID] = post; return nil })
}

package paddock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GitHubIdentity struct {
	Mode  string           `json:"mode"`
	Login string           `json:"login"`
	App   *GitHubAppConfig `json:"app,omitempty"`
}

type PullRequest struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type GitHubComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	HTMLURL   string    `json:"html_url"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	InReplyTo int64     `json:"in_reply_to_id,omitempty"`
	Kind      string    `json:"kind"`
}

var githubRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9_-]+(\[bot\])?$`)

func ParsePullRequest(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", 0, fmt.Errorf("expected https://github.com/owner/repo/pull/number")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || !githubRepoName.MatchString(parts[0]+"/"+parts[1]) {
		return "", 0, fmt.Errorf("invalid pull request URL")
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("invalid pull request number")
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), n, nil
}

// GitHubAPI owns transport identity only. It has no authority to select a task,
// approve an author, or decide which action an agent should take.
type GitHubAPI struct {
	Identity GitHubIdentity
	app      *githubAppAuth
}

func NewGitHubAPI(identity GitHubIdentity) GitHubAPI {
	g := GitHubAPI{Identity: identity}
	if identity.Mode == "app" && identity.App != nil {
		g.app = newGitHubAppAuth(*identity.App)
	}
	return g
}

func (g GitHubAPI) AuthenticatedIdentity(ctx context.Context) (GitHubIdentity, error) {
	identity := g.Identity
	if identity.Mode == "app" {
		if g.app == nil {
			return identity, fmt.Errorf("configure App ID, installation ID and private key; external token mode has been replaced")
		}
		login, err := g.app.authenticate(ctx, identity.Login)
		identity.Login = login
		return identity, err
	}
	return identity, g.ValidateIdentity(ctx)
}

func (g GitHubAPI) request(ctx context.Context, method, path string, body, result any) error {
	if !strings.HasPrefix(path, "repos/") && !strings.HasPrefix(path, "users/") && path != "user" && path != "installation/repositories?per_page=1" {
		return fmt.Errorf("unsupported GitHub API path")
	}
	if g.Identity.Mode == "app" {
		if g.app == nil {
			return fmt.Errorf("configure App credentials with paddock github auth")
		}
		return g.app.request(ctx, method, path, body, result)
	}
	args := []string{"api", "--hostname", "github.com", "--method", method, "-H", "Accept: application/vnd.github+json", path}
	var input []byte
	if body != nil {
		var err error
		input, err = json.Marshal(body)
		if err != nil {
			return err
		}
		args = append(args, "--input", "-")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(input)
	env := credentialFreeEnvironment(os.Environ())
	if g.Identity.Mode != "gh" {
		return fmt.Errorf("configure GitHub identity first")
	}
	cmd.Env = append(env, "GH_PROMPT_DISABLED=1")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("GitHub %s %s failed: %w", method, path, err)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(output, result)
}

func (g GitHubAPI) ValidateIdentity(ctx context.Context) error {
	if g.Identity.Mode == "app" {
		_, err := g.AuthenticatedIdentity(ctx)
		return err
	}
	if !githubLogin.MatchString(g.Identity.Login) {
		return fmt.Errorf("invalid GitHub identity login")
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := g.request(ctx, "GET", "user", nil, &user); err != nil {
		return err
	}
	if !strings.EqualFold(user.Login, g.Identity.Login) {
		return fmt.Errorf("gh is authenticated as %s, not configured identity %s", user.Login, g.Identity.Login)
	}
	return nil
}
func (g GitHubAPI) Pull(ctx context.Context, repo string, n int) (PullRequest, error) {
	var pr PullRequest
	if !githubRepoName.MatchString(repo) || n < 1 {
		return pr, fmt.Errorf("invalid repository or PR")
	}
	err := g.request(ctx, "GET", fmt.Sprintf("repos/%s/pulls/%d", repo, n), nil, &pr)
	return pr, err
}
func (g GitHubAPI) UserID(ctx context.Context, login string) (int64, error) {
	if !githubLogin.MatchString(login) {
		return 0, fmt.Errorf("invalid authorized login")
	}
	var user struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	err := g.request(ctx, "GET", "users/"+url.PathEscape(login), nil, &user)
	if err == nil && (user.ID < 1 || user.Type != "User") {
		err = fmt.Errorf("authorized engineer must be a GitHub user")
	}
	return user.ID, err
}
func (g GitHubAPI) Comments(ctx context.Context, repo string, n int) ([]GitHubComment, error) {
	if !githubRepoName.MatchString(repo) || n < 1 {
		return nil, fmt.Errorf("invalid repository or PR")
	}
	var all []GitHubComment
	for _, kind := range []string{"discussion", "review"} {
		resource := "issues"
		if kind == "review" {
			resource = "pulls"
		}
		for page := 1; page <= 10000; page++ {
			var comments []GitHubComment
			path := fmt.Sprintf("repos/%s/%s/%d/comments?per_page=100&page=%d", repo, resource, n, page)
			if err := g.request(ctx, "GET", path, nil, &comments); err != nil {
				return nil, err
			}
			for _, comment := range comments {
				comment.Kind = kind
				all = append(all, comment)
			}
			if len(comments) < 100 {
				break
			}
			if page == 10000 {
				return nil, fmt.Errorf("comment pagination limit reached")
			}
		}
	}
	return all, nil
}
func (g GitHubAPI) Reply(ctx context.Context, repo string, n int, comment GitHubComment, body string) (GitHubComment, error) {
	var result GitHubComment
	if !githubRepoName.MatchString(repo) || n < 1 {
		return result, fmt.Errorf("invalid repository or PR")
	}
	path := fmt.Sprintf("repos/%s/issues/%d/comments", repo, n)
	if comment.Kind == "review" {
		root := comment.ID
		if comment.InReplyTo != 0 {
			root = comment.InReplyTo
		}
		if root < 1 {
			return result, fmt.Errorf("invalid review comment")
		}
		path = fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", repo, n, root)
	}
	err := g.request(ctx, "POST", path, map[string]string{"body": body}, &result)
	return result, err
}

// GitHub returns the existing reaction when this identity already added eyes.
// Retrying after a lost response is safe and does not create another reaction.
func (g GitHubAPI) ReactEyes(ctx context.Context, repo string, comment GitHubComment) error {
	if !githubRepoName.MatchString(repo) || comment.ID < 1 {
		return fmt.Errorf("invalid repository or comment")
	}
	resource := "issues"
	switch comment.Kind {
	case "review":
		resource = "pulls"
	case "discussion":
	default:
		return fmt.Errorf("unsupported comment kind")
	}
	path := fmt.Sprintf("repos/%s/%s/comments/%d/reactions", repo, resource, comment.ID)
	return g.request(ctx, "POST", path, map[string]string{"content": "eyes"}, nil)
}

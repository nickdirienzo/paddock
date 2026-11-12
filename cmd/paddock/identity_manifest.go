package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func appManifest(name, redirect string, write, public bool) map[string]any {
	level := "read"
	if write {
		level = "write"
	}
	return map[string]any{
		"name": name, "url": "https://github.com/nickdirienzo/paddock",
		"description":     "A Paddock coding agent identity",
		"hook_attributes": map[string]any{"url": "https://github.com/nickdirienzo/paddock", "active": false},
		"redirect_url":    redirect, "public": public, "request_oauth_on_install": false,
		"default_events":      []string{},
		"default_permissions": map[string]string{"contents": level, "pull_requests": level, "issues": level},
	}
}

func manifestConversion(ctx context.Context, code string) (int64, string, []byte, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return manifestConversionAt(ctx, code, "https://api.github.com", client)
}

func manifestConversionAt(ctx context.Context, code, baseURL string, client *http.Client) (int64, string, []byte, error) {
	if len(code) < 8 || len(code) > 256 || strings.ContainsAny(code, "/?&# ") {
		return 0, "", nil, fmt.Errorf("invalid GitHub manifest code")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("GitHub manifest conversion failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return 0, "", nil, fmt.Errorf("GitHub manifest conversion returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		PEM  string `json:"pem"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil || result.ID < 1 || result.Slug == "" || result.PEM == "" {
		return 0, "", nil, fmt.Errorf("GitHub manifest conversion returned incomplete App credentials")
	}
	return result.ID, result.Slug, []byte(result.PEM), nil
}

func runGitHubManifestSetup(name string, args []string, out io.Writer) error {
	flags, pos, err := parse(args, "owner app-name visibility")
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("setup takes no extra arguments")
	}
	visibility := flags["visibility"]
	if visibility == "" {
		visibility = "owner"
	}
	if visibility != "owner" && visibility != "any" {
		return fmt.Errorf("visibility must be owner or any")
	}
	owner := flags["owner"]
	if owner != "" && (!githubOwnerName(owner)) {
		return fmt.Errorf("invalid GitHub organization name")
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
	identity, exists := state.Identities[name]
	if !exists || (identity.Owner != "" && identity.Owner != caller) {
		return fmt.Errorf("create and own identity %q first", name)
	}
	if identity.Mode != "" {
		return fmt.Errorf("identity %q already has GitHub credentials; rotate or detach them first", name)
	}
	appName := flags["app-name"]
	if appName == "" {
		appName = "paddock-" + name
	}
	if len(appName) > 100 {
		return fmt.Errorf("App name is too long")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("cannot start temporary GitHub registration callback: %w", err)
	}
	defer listener.Close()
	base := "http://" + listener.Addr().String()
	secret := make([]byte, 24)
	if _, err = rand.Read(secret); err != nil {
		return err
	}
	csrf := hex.EncodeToString(secret)
	endpoint := "https://github.com/settings/apps/new?state=" + csrf
	if owner != "" {
		endpoint = "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/apps/new?state=" + csrf
	}
	write := len(identity.Profiles) == 0
	for _, p := range identity.Profiles {
		if p == "implementation" {
			write = true
		}
	}
	manifest, err := json.Marshal(appManifest(appName, base+"/callback", write, visibility == "any"))
	if err != nil {
		return err
	}
	code := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, `<h1>Register Paddock GitHub App</h1><p>Review the App name, permissions, and repository access on GitHub before creating it.</p><form method="post" action="%s"><input type="hidden" name="manifest" value="%s"><button type="submit">Continue to GitHub</button></form>`, htmlEscape(endpoint), htmlEscape(string(manifest)))
	})
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != csrf {
			http.Error(w, "registration state mismatch", http.StatusForbidden)
			return
		}
		value := r.URL.Query().Get("code")
		if value == "" {
			http.Error(w, "missing GitHub code", http.StatusBadRequest)
			return
		}
		select {
		case code <- value:
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "GitHub registration received. Return to the Paddock terminal.\n")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	_, _ = fmt.Fprintln(out, "Open", base+"/", "in your browser. Paddock will wait for GitHub's callback (up to one hour).")
	var received string
	select {
	case received = <-code:
	case <-time.After(time.Hour):
		return fmt.Errorf("GitHub App registration expired; run setup again")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	appID, slug, pem, err := manifestConversion(ctx, received)
	if err != nil {
		return err
	}
	defer clear(pem)
	keyPath, err := paddock.ImportGitHubAppKeyBytesForIdentity(appID, name, pem)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Created GitHub App %s (ID %d). Private key imported to protected host storage.\n", slug, appID)
	_, _ = fmt.Fprintf(out, "Install at https://github.com/apps/%s/installations/new and select the authorized repositories.\n", slug)
	_, _ = fmt.Fprintf(out, "Then run: paddock github installations --app-id %d --private-key-file %q\n", appID, keyPath)
	_, _ = fmt.Fprintf(out, "Finish with: paddock identity github associate %s --app-id %d --installation-id ID --private-key-file %q\n", name, appID, keyPath)
	return nil
}

func githubOwnerName(s string) bool {
	if len(s) == 0 || len(s) > 39 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if c != '-' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;").Replace(s)
}

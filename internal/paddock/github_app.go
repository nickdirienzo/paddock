package paddock

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Public credential references only. JWTs, private keys and installation tokens
// live in the authentication module and never enter the filesystem event journal.
type GitHubAppConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	KeyFile        string `json:"key_file,omitempty"`
	KeyEnvironment bool   `json:"key_environment,omitempty"`
}

type githubAppAuth struct {
	config  GitHubAppConfig
	mu      sync.Mutex
	token   string
	expires time.Time
	client  *http.Client
	baseURL string
	clock   func() time.Time
}

func newGitHubAppAuth(config GitHubAppConfig) *githubAppAuth {
	return &githubAppAuth{config: config, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, baseURL: "https://api.github.com", clock: time.Now}
}
func (a *githubAppAuth) key() (*rsa.PrivateKey, error) {
	if a.config.AppID < 1 {
		return nil, fmt.Errorf("App ID must be a positive integer")
	}
	var data []byte
	var err error
	if a.config.KeyEnvironment {
		data = []byte(os.Getenv("PADDOCK_GITHUB_PRIVATE_KEY"))
		if len(data) == 0 {
			return nil, fmt.Errorf("PADDOCK_GITHUB_PRIVATE_KEY is unset")
		}
	} else {
		if a.config.KeyFile == "" {
			return nil, fmt.Errorf("configure an App private key file or PADDOCK_GITHUB_PRIVATE_KEY")
		}
		data, err = readProtectedKey(a.config.KeyFile)
		if err != nil {
			return nil, err
		}
	}
	defer clear(data)
	return parseAppKey(data)
}
func parseAppKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("private key must contain one unencrypted RSA PEM block")
	}
	defer clear(block.Bytes)
	var key *rsa.PrivateKey
	if block.Type == "RSA PRIVATE KEY" {
		key, _ = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else if block.Type == "PRIVATE KEY" {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			key, _ = parsed.(*rsa.PrivateKey)
		}
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("private key must be an RSA key of at least 2048 bits")
	}
	if key.Validate() != nil {
		return nil, fmt.Errorf("invalid RSA private key")
	}
	return key, nil
}
func readProtectedKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read configured private key file")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("private key must be a regular file, not a symlink")
	}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("use environment-based App credentials on Windows; Unix file permissions are required for file storage")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private key file must have owner-only permissions (chmod 600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open private key file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private key file changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("cannot read private key PEM")
	}
	return data, nil
}

// Import a downloaded key into a separate owner-only credential directory.
// The original file remains untouched. Never place this directory in task state.
func ImportGitHubAppKey(appID int64, source string) (string, error) {
	if appID < 1 {
		return "", fmt.Errorf("invalid App ID")
	}
	data, err := readProtectedKey(source)
	if err != nil {
		return "", err
	}
	defer clear(data)
	if _, err = parseAppKey(data); err != nil {
		return "", err
	}
	home, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "paddock", "credentials")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create credential directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("credential directory must be a real directory with owner-only permissions")
	}
	f, err := os.CreateTemp(dir, ".github-app-*.pem")
	if err != nil {
		return "", fmt.Errorf("cannot create protected credential file")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return "", fmt.Errorf("cannot store private key")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "github-app-"+strconv.FormatInt(appID, 10)+".pem")
	if err = replaceFile(f.Name(), dest); err != nil {
		return "", fmt.Errorf("cannot install private key")
	}
	if err = syncDirectory(dir); err != nil {
		return "", err
	}
	return dest, nil
}
func (a *githubAppAuth) jwt() (string, error) {
	key, err := a.key()
	if err != nil {
		return "", err
	}
	now := a.clock()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iss": a.config.AppID, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	signed := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("could not sign GitHub App JWT")
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
func (a *githubAppAuth) http(ctx context.Context, method, path, credential string, body, result any) (int, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("cannot encode GitHub request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+"/"+path, bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("cannot construct GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "paddock")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub request failed (transport error)")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("GitHub %s failed (HTTP %d)", method, response.StatusCode)
	}
	if result != nil && json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(result) != nil {
		return response.StatusCode, fmt.Errorf("invalid GitHub response")
	}
	return response.StatusCode, nil
}
func (a *githubAppAuth) installationToken(ctx context.Context) (string, error) {
	if a.config.InstallationID < 1 {
		return "", fmt.Errorf("installation ID must be a positive integer")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.expires.After(a.clock().Add(time.Minute)) {
		return a.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err = a.http(ctx, "POST", fmt.Sprintf("app/installations/%d/access_tokens", a.config.InstallationID), jwt, struct{}{}, &result)
	if err != nil {
		return "", err
	}
	if result.Token == "" || !result.ExpiresAt.After(a.clock().Add(time.Minute)) {
		return "", fmt.Errorf("GitHub returned an invalid or already-expiring installation token")
	}
	a.token = result.Token
	a.expires = result.ExpiresAt
	return a.token, nil
}
func (a *githubAppAuth) request(ctx context.Context, method, path string, body, result any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.installationToken(ctx)
		if err != nil {
			return err
		}
		status, err := a.http(ctx, method, path, token, body, result)
		if status != http.StatusUnauthorized || attempt == 1 {
			return err
		}
		// A confirmed 401 rejected authentication before the operation. Only this
		// response permits a retry; timeouts/5xx are never replayed for mutations.
		a.mu.Lock()
		if a.token == token {
			a.token = ""
			a.expires = time.Time{}
		}
		a.mu.Unlock()
	}
	return fmt.Errorf("installation token refresh failed")
}
func (a *githubAppAuth) authenticate(ctx context.Context, expected string) (string, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if _, err = a.http(ctx, "GET", "app", jwt, nil, &app); err != nil {
		return "", err
	}
	login := app.Slug + "[bot]"
	if app.ID != a.config.AppID || app.Slug == "" || !githubLogin.MatchString(login) {
		return "", fmt.Errorf("GitHub App identity did not match configured App ID")
	}
	if expected != "" && !strings.EqualFold(expected, login) {
		return "", fmt.Errorf("configured bot login does not match the authenticated GitHub App")
	}
	var installation struct {
		ID          int64   `json:"id"`
		AppID       int64   `json:"app_id"`
		SuspendedAt *string `json:"suspended_at"`
	}
	if _, err = a.http(ctx, "GET", fmt.Sprintf("app/installations/%d", a.config.InstallationID), jwt, nil, &installation); err != nil {
		return "", err
	}
	if installation.ID != a.config.InstallationID || installation.AppID != a.config.AppID || installation.SuspendedAt != nil {
		return "", fmt.Errorf("installation is suspended or belongs to a different GitHub App")
	}
	if err = a.request(ctx, "GET", "installation/repositories?per_page=1", nil, nil); err != nil {
		return "", err
	}
	return login, nil
}

type GitHubAppInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

func DiscoverGitHubAppInstallations(ctx context.Context, config GitHubAppConfig) ([]GitHubAppInstallation, error) {
	a := newGitHubAppAuth(config)
	jwt, err := a.jwt()
	if err != nil {
		return nil, err
	}
	var all []GitHubAppInstallation
	for page := 1; page <= 100; page++ {
		var entries []GitHubAppInstallation
		if _, err = a.http(ctx, "GET", fmt.Sprintf("app/installations?per_page=100&page=%d", page), jwt, nil, &entries); err != nil {
			return nil, err
		}
		all = append(all, entries...)
		if len(entries) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("installation pagination limit reached")
}

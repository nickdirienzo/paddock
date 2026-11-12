package paddock

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func appKeyFixture(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
func TestGitHubAppJWTRefreshAndIdentity(t *testing.T) {
	key, keyPEM := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(keyPEM))
	clock := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	minted, operations := 0, 0
	rejectOnce := false
	failServer := false
	validateJWT := func(token string) {
		t.Helper()
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Error("not a JWT")
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Error(err)
			return
		}
		var claims struct {
			Issuer  int64 `json:"iss"`
			Issued  int64 `json:"iat"`
			Expires int64 `json:"exp"`
		}
		if json.Unmarshal(payload, &claims) != nil || claims.Issuer != 42 || claims.Issued != clock.Add(-time.Minute).Unix() || claims.Expires != clock.Add(9*time.Minute).Unix() {
			t.Error("incorrect JWT claims")
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Error(err)
			return
		}
		hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], signature) != nil {
			t.Error("invalid JWT signature")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.Contains(token, "PRIVATE KEY") {
			t.Error("private key sent over HTTP")
		}
		switch r.URL.Path {
		case "/app":
			validateJWT(token)
			fmt.Fprint(w, `{"id":42,"slug":"team-robot"}`)
		case "/app/installations/99":
			validateJWT(token)
			fmt.Fprint(w, `{"id":99,"app_id":42,"suspended_at":null}`)
		case "/app/installations/99/access_tokens":
			validateJWT(token)
			minted++
			json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_fixture_%d", minted), "expires_at": clock.Add(time.Hour)})
		default:
			operations++
			if failServer {
				w.WriteHeader(502)
				fmt.Fprint(w, "ghs_should_never_be_logged")
				return
			}
			if rejectOnce {
				rejectOnce = false
				w.WriteHeader(401)
				return
			}
			if token != fmt.Sprintf("ghs_fixture_%d", minted) {
				t.Error("wrong installation token")
			}
			fmt.Fprint(w, `{"repositories":[]}`)
		}
	}))
	defer server.Close()
	a := newGitHubAppAuth(GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true})
	a.baseURL = server.URL
	a.clock = func() time.Time { return clock }
	login, err := a.authenticate(context.Background(), "")
	if err != nil || login != "team-robot[bot]" {
		t.Fatal(login, err)
	}
	if minted != 1 {
		t.Fatal("expected initial mint", minted)
	}
	if err = a.request(context.Background(), "GET", "repos/team/repo/pulls/1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if minted != 1 {
		t.Fatal("did not reuse in-memory token")
	}
	clock = clock.Add(time.Hour)
	if err = a.request(context.Background(), "GET", "repos/team/repo/pulls/1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if minted != 2 {
		t.Fatal("did not refresh expired token")
	}
	mu.Lock()
	rejectOnce = true
	mu.Unlock()
	if err = a.request(context.Background(), "POST", "repos/team/repo/issues/1/comments", map[string]string{"body": "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if minted != 3 {
		t.Fatal("did not recover after 401")
	}
	mu.Lock()
	failServer = true
	before := operations
	mu.Unlock()
	err = a.request(context.Background(), "POST", "repos/team/repo/issues/1/comments", nil, nil)
	if err == nil || strings.Contains(err.Error(), "ghs_") || operations != before+1 {
		t.Fatal("unsafe error or mutation retry", err)
	}
	if _, err = a.authenticate(context.Background(), "wrong[bot]"); err == nil {
		t.Fatal("accepted wrong bot identity")
	}
}

func TestGitHubAppConcurrentMintAndRedirect(t *testing.T) {
	_, keyPEM := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(keyPEM))
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/99/access_tokens" {
			mu.Lock()
			count++
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		http.Redirect(w, r, "https://example.invalid/secret", 302)
	}))
	defer server.Close()
	a := newGitHubAppAuth(GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true})
	a.baseURL = server.URL
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, err := a.installationToken(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if count != 1 {
		t.Fatal("concurrent refresh minted multiple tokens", count)
	}
	if err := a.request(context.Background(), "GET", "repos/team/repo", nil, nil); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatal("redirect not rejected", err)
	}
}
func TestGitHubAppProtectedKeyStorage(t *testing.T) {
	_, keyPEM := appKeyFixture(t)
	source := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(source, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedKey(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedKey(source); err == nil {
		t.Fatal("accepted world-readable key")
	}
	os.Chmod(source, 0600)
	link := filepath.Join(t.TempDir(), "link.pem")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedKey(link); err == nil {
		t.Fatal("accepted symlink")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := ImportGitHubAppKey(42, source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(keyPEM) {
		t.Fatal("stored key changed", err)
	}
	identity := GitHubIdentity{Mode: "app", Login: "team[bot]", App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyFile: path}}
	serialized, err := json.Marshal(identity)
	if err != nil || strings.Contains(string(serialized), "PRIVATE KEY") || strings.Contains(string(serialized), "ghs_") {
		t.Fatal("secret persisted")
	}
}
func TestGitHubAppKeyFormats(t *testing.T) {
	key, _ := appKeyFixture(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseAppKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Fatal(err)
	}
	if _, err := parseAppKey([]byte("secret-invalid-key")); err == nil || strings.Contains(err.Error(), "secret-invalid-key") {
		t.Fatal("unsafe parse error")
	}
}

func TestNamedIdentityKeysDoNotOverwriteEachOther(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, first := appKeyFixture(t)
	_, second := appKeyFixture(t)
	one, err := ImportGitHubAppKeyBytesForIdentity(42, "mozzie", first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ImportGitHubAppKeyBytesForIdentity(42, "seamus", first)
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("identity key paths alias")
	}
	if _, err := ImportGitHubAppKeyBytesForIdentity(42, "mozzie", second); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(two)
	if err != nil || string(retained) != string(first) {
		t.Fatal("other identity key changed", err)
	}
	if err := RemoveGitHubAppKeyForIdentity(42, "mozzie", one); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(one); !os.IsNotExist(err) {
		t.Fatal("detached key remains", err)
	}
	if _, err := os.Stat(two); err != nil {
		t.Fatal("other identity key removed", err)
	}
}

func TestInstallationRepositoryInventory(t *testing.T) {
	_, pem := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(pem))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_fixture", "expires_at": time.Now().Add(time.Hour)})
		case "/installation/repositories":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
				t.Error("wrong pagination")
			}
			fmt.Fprint(w, `{"repositories":[{"full_name":"owner/one"},{"full_name":"owner/two"}]}`)
		case "/users/mozziebot%5Bbot%5D", "/users/mozziebot[bot]":
			fmt.Fprint(w, `{"id":351659889,"login":"mozziebot[bot]","type":"Bot"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	api := NewGitHubAPI(GitHubIdentity{Mode: "app", App: &GitHubAppConfig{AppID: 42, InstallationID: 99, KeyEnvironment: true}})
	api.app.baseURL = server.URL
	repos, err := api.InstallationRepositories(context.Background())
	if err != nil || len(repos) != 2 || repos[0] != "owner/one" || repos[1] != "owner/two" {
		t.Fatalf("repositories: %v %v", repos, err)
	}
	botID, err := api.BotUserID(context.Background(), "mozziebot[bot]")
	if err != nil || botID != 351659889 {
		t.Fatalf("bot actor: %d %v", botID, err)
	}
}

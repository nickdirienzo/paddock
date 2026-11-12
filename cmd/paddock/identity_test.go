package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickdirienzo/paddock/internal/paddock"
)

func TestIdentityCLIWithoutGitHub(t *testing.T) {
	t.Setenv("PADDOCK_HOME", t.TempDir())
	var out bytes.Buffer
	if err := runIdentity([]string{"create", "mozzie", "--profiles", "investigation"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runIdentity([]string{"show", "mozzie", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Mode     string   `json:"mode"`
		Owner    string   `json:"owner"`
		Profiles []string `json:"profiles"`
	}
	if err := json.Unmarshal(out.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	if identity.Mode != "" || identity.Owner == "" || len(identity.Profiles) != 1 || identity.Profiles[0] != "investigation" {
		t.Fatalf("incorrect logical identity: %+v", identity)
	}
}

func TestRotatePreservesGitHubPrincipal(t *testing.T) {
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY_FILE", "")
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", "")
	identity := paddock.GitHubIdentity{Mode: "app", Login: "helper[bot]", App: &paddock.GitHubAppConfig{AppID: 12, InstallationID: 34}}
	for _, args := range [][]string{
		{"--app-id", "13", "--private-key-file", "/tmp/new.pem"},
		{"--installation-id", "35", "--private-key-file", "/tmp/new.pem"},
		{"--login", "another[bot]", "--private-key-file", "/tmp/new.pem"},
	} {
		if _, err := rotationAuthArgs("helper", identity, args); err == nil {
			t.Fatalf("accepted principal change: %v", args)
		}
	}
	if _, err := rotationAuthArgs("helper", identity, nil); err == nil {
		t.Fatal("accepted rotation without new key")
	}
	args, err := rotationAuthArgs("helper", identity, []string{"--private-key-file", "/tmp/new.pem"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"--app-id 12", "--installation-id 34", "--login helper[bot]", "--private-key-file /tmp/new.pem"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("rotation omitted %s: %v", required, args)
		}
	}
}

func TestAssociateRequiresUnbackedIdentity(t *testing.T) {
	t.Setenv("PADDOCK_HOME", t.TempDir())
	home, err := paddock.StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := paddock.LocalCaller()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("mozzie", caller, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateGitHub(func(v *paddock.GitHubState) error {
		identity := v.Identities["mozzie"]
		identity.Mode = "app"
		identity.Login = "mozziebot[bot]"
		identity.App = &paddock.GitHubAppConfig{AppID: 12, InstallationID: 34}
		v.Identities["mozzie"] = identity
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if err := runIdentityGitHub([]string{"associate", "mozzie"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already has GitHub credentials") {
		t.Fatalf("associate replaced an existing App: %v", err)
	}
}

func TestGitHubCLIHidesAndProtectsOtherOwners(t *testing.T) {
	t.Setenv("PADDOCK_HOME", t.TempDir())
	home, err := paddock.StateHome()
	if err != nil {
		t.Fatal(err)
	}
	store, err := paddock.OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIdentity("other", "999999:other", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateGitHub(func(v *paddock.GitHubState) error {
		v.Routes["team/project"] = "other"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	var out bytes.Buffer
	if err := runGitHub([]string{"identities"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "other") {
		t.Fatalf("other owner's identity was listed: %s", out.String())
	}
	if err := runGitHub([]string{"unroute", "team/project"}, &out); err == nil {
		t.Fatal("removed another owner's route")
	}
}

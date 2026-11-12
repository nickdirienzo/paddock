package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppManifestPermissionsAndCallback(t *testing.T) {
	if !githubOwnerName("engineering-team") || githubOwnerName("bad_org") || githubOwnerName("-bad") {
		t.Fatal("organization validation")
	}
	read := appManifest("mozziebot", "http://127.0.0.1:9999/callback", false, false)
	if read["public"] != false || read["redirect_url"] != "http://127.0.0.1:9999/callback" {
		t.Fatal("incorrect registration scope")
	}
	permissions := read["default_permissions"].(map[string]string)
	for _, value := range permissions {
		if value != "read" {
			t.Fatal("investigation App has write permission")
		}
	}
	write := appManifest("mozziebot", "http://127.0.0.1:9999/callback", true, true)
	for _, value := range write["default_permissions"].(map[string]string) {
		if value != "write" {
			t.Fatal("implementation App lacks write permission")
		}
	}
}

func TestManifestConversionUsesOneTimeCodeWithoutLeakingPEM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/temporary-code/conversions" || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("wrong conversion request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":42,"slug":"mozziebot","pem":"private-test-value"}`)
	}))
	defer server.Close()
	id, slug, pem, err := manifestConversionAt(context.Background(), "temporary-code", server.URL, server.Client())
	if err != nil || id != 42 || slug != "mozziebot" || string(pem) != "private-test-value" {
		t.Fatalf("conversion: %d %s %v", id, slug, err)
	}
	if _, _, _, err = manifestConversionAt(context.Background(), "bad/code", server.URL, server.Client()); err == nil || strings.Contains(err.Error(), "private-test-value") {
		t.Fatal("unsafe conversion error")
	}
}

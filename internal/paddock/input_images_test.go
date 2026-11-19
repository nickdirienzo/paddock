package paddock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func imageManifestFixture(t *testing.T) (string, string, []InputImage) {
	t.Helper()
	dir := t.TempDir()
	data, _ := base64.StdEncoding.DecodeString(clipboardPNG)
	source := filepath.Join(dir, "screenshot.png")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "images.json")
	raw, _ := json.Marshal([]string{source})
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	images, err := LoadInputImages(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, source, images
}

func TestInputImageSnapshotAndIdempotency(t *testing.T) {
	manifest, source, images := imageManifestFixture(t)
	original := append([]byte{}, images[0].data...)
	body := ImageMessageBody("Inspect the screenshot", images)
	if strings.Contains(body, source) || !strings.Contains(body, sandboxInputImages+"/"+images[0].Name) {
		t.Fatal("body contains host path or omits image")
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := Task{ID: newID()}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	r, claimed, err := store.ClaimAuthoredMessage(task.ID, "caller", "image-request", body)
	if err != nil || !claimed {
		t.Fatal(r, claimed, err)
	}
	if _, err := store.FinishMessage(r, "pending", "", "offline"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("source changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(images[0].data, original) {
		t.Fatal("snapshot changed")
	}
	if _, err := LoadInputImages(manifest); err == nil {
		t.Fatal("accepted non-image bytes")
	}
	changed := append([]byte{}, original...)
	changed = append(changed, 'x')
	other, err := inputImage(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimAuthoredMessage(task.ID, "caller", "image-request", ImageMessageBody("Inspect the screenshot", []InputImage{other})); err == nil {
		t.Fatal("idempotency ignored changed image bytes")
	}
	if _, claimed, err := store.ClaimAuthoredMessage(task.ID, "caller", "image-request", body); err != nil || !claimed {
		t.Fatal("same image retry rejected", err)
	}
}

func TestInputImageManifestRejectsLinksAndLimits(t *testing.T) {
	manifest, source, _ := imageManifestFixture(t)
	write := func(paths []string) {
		raw, _ := json.Marshal(paths)
		if err := os.WriteFile(manifest, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := source + ".link"
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	write([]string{link})
	if _, err := LoadInputImages(manifest); err == nil {
		t.Fatal("accepted symlink")
	}
	paths := make([]string, maxInputImages+1)
	for i := range paths {
		paths[i] = source
	}
	write(paths)
	if _, err := LoadInputImages(manifest); err == nil {
		t.Fatal("accepted too many images")
	}
	file, err := os.OpenFile(source, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxInputImageBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	write([]string{source})
	if _, err := LoadInputImages(manifest); err == nil {
		t.Fatal("accepted oversized image")
	}
}

func TestTaskImagesSurviveSourceRemovalAndStageBeforeDelivery(t *testing.T) {
	_, source, images := imageManifestFixture(t)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// A deterministic OpenShell fixture copies the uploaded bytes and records
	// the destination; no live sandbox or model is used.
	dir := t.TempDir()
	binary := filepath.Join(dir, "openshell")
	script := "#!/bin/sh\nshift 4\ncase \"$1 $2\" in\n  'sandbox exec') exit 0;;\n  'sandbox upload') cp \"$4\" '" + dir + "/uploaded'; printf '%s' \"$5\" > '" + dir + "/destination';;\n  *) exit 1;;\nesac\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	task := Task{ID: newID(), Runtime: "openshell", Worktree: "/sandbox/worktree", InputImages: images, OpenShell: &OpenShellTask{Name: "sandbox-one", Config: OpenShellConfig{Binary: binary, Gateway: "test", Workspace: "test"}}}
	s := Service{Store: store}
	if err := s.saveTaskImages(task); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(task); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	task, err = store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.InputImages) != 1 || len(task.InputImages[0].data) != 0 {
		t.Fatal("image bytes leaked into task JSON")
	}
	if err := s.stageTaskImages(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(filepath.Join(dir, "uploaded"))
	if !bytes.Equal(actual, images[0].data) {
		t.Fatal("uploaded different bytes")
	}
	destination, _ := os.ReadFile(filepath.Join(dir, "destination"))
	if string(destination) != sandboxInputImages+"/"+images[0].Name {
		t.Fatal("wrong sandbox destination", string(destination))
	}
	if err := os.WriteFile(filepath.Join(store.Home, "input-images", task.ID, images[0].Name), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.stageTaskImages(context.Background(), task); err == nil {
		t.Fatal("staged altered cached bytes")
	}
	if err := stageInputImages(context.Background(), Task{}, images); err == nil {
		t.Fatal("accepted host fallback")
	}
}

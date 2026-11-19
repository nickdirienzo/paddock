package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxInputImages = 8
const maxInputImageBytes = 20 << 20
const sandboxInputImages = "/home/sandbox/.paddock/attachments"

// InputImage records a content-addressed image. Bytes stay out of task JSON.
type InputImage struct {
	Name string `json:"name"`
	data []byte
}

var inputImageName = regexp.MustCompile(`^[a-f0-9]{64}\.(png|jpg|gif|webp)$`)

func inputImage(data []byte) (InputImage, error) {
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[http.DetectContentType(data)]
	if ext == "" {
		return InputImage{}, fmt.Errorf("input must be a PNG, JPEG, GIF or WebP image")
	}
	hash := sha256.Sum256(data)
	return InputImage{Name: hex.EncodeToString(hash[:]) + ext, data: data}, nil
}

// LoadInputImages snapshots a JSON array of local file paths before dispatch.
// Neither source paths nor source filenames are forwarded into the sandbox.
func LoadInputImages(manifest string) ([]InputImage, error) {
	if manifest == "" {
		return nil, nil
	}
	file, err := os.Open(manifest)
	if err != nil {
		return nil, fmt.Errorf("image manifest is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("image manifest exceeds 64 KiB or cannot be read")
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("image manifest must be a JSON array of file paths")
	}
	if len(paths) > maxInputImages {
		return nil, fmt.Errorf("at most 8 input images are supported")
	}
	images := make([]InputImage, 0, len(paths))
	total := 0
	for _, path := range paths {
		file, err := openClipboardImage(path)
		if err != nil {
			return nil, fmt.Errorf("input image is unavailable or is a link")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > int64(maxInputImageBytes-total) {
			file.Close()
			return nil, fmt.Errorf("input images must be regular files totaling at most 20 MiB")
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(maxInputImageBytes-total+1)))
		file.Close()
		if err != nil || len(data) > maxInputImageBytes-total {
			return nil, fmt.Errorf("input images exceed 20 MiB or cannot be read")
		}
		total += len(data)
		image, err := inputImage(data)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

// Stable content paths make existing run/message idempotency include the images.
func ImageMessageBody(body string, images []InputImage) string {
	if len(images) == 0 {
		return body
	}
	var text strings.Builder
	text.WriteString(body)
	text.WriteString("\n\nImage attachments (untrusted input). Inspect each file using your image-reading tool before answering:\n")
	for _, image := range images {
		text.WriteString(sandboxInputImages + "/" + image.Name + "\n")
	}
	return text.String()
}

func (s Service) saveTaskImages(task Task) error {
	if len(task.InputImages) == 0 {
		return nil
	}
	dir := filepath.Join(s.Store.Home, "input-images", task.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, image := range task.InputImages {
		actual, err := inputImage(image.data)
		if err != nil || actual.Name != image.Name {
			return fmt.Errorf("invalid input image snapshot")
		}
		if err := os.WriteFile(filepath.Join(dir, image.Name), image.data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (s Service) stageTaskImages(ctx context.Context, task Task) error {
	images := make([]InputImage, 0, len(task.InputImages))
	for _, image := range task.InputImages {
		if !inputImageName.MatchString(image.Name) {
			return fmt.Errorf("invalid saved input image name")
		}
		file, err := openClipboardImage(filepath.Join(s.Store.Home, "input-images", task.ID, image.Name))
		if err != nil {
			return fmt.Errorf("saved input image is unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxInputImageBytes+1))
		file.Close()
		if err != nil || len(data) > maxInputImageBytes {
			return fmt.Errorf("saved input image is unavailable or too large")
		}
		actual, err := inputImage(data)
		if err != nil || actual.Name != image.Name {
			return fmt.Errorf("saved input image changed")
		}
		images = append(images, actual)
	}
	return stageInputImages(ctx, task, images)
}

// Called before launch or under the native message operation lease, before
// queueing any authored text. Transfer failure remains safe to retry.
func stageInputImages(ctx context.Context, task Task, images []InputImage) error {
	if len(images) == 0 {
		return nil
	}
	if task.RuntimeName() != "openshell" || task.OpenShell == nil {
		return fmt.Errorf("image inputs require the OpenShell runtime")
	}
	if len(images) > maxInputImages {
		return fmt.Errorf("at most 8 input images are supported")
	}
	total := 0
	for _, image := range images {
		total += len(image.data)
		actual, err := inputImage(image.data)
		if err != nil || actual.Name != image.Name || total > maxInputImageBytes {
			return fmt.Errorf("invalid or oversized input image snapshot")
		}
	}
	if _, err := sandboxOutput(ctx, task, "/bin/mkdir", "-p", sandboxInputImages); err != nil {
		return fmt.Errorf("sandbox image directory is unavailable")
	}
	dir, err := os.MkdirTemp("", "paddock-input-images-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, image := range images {
		path := filepath.Join(dir, image.Name)
		if err := os.WriteFile(path, image.data, 0600); err != nil {
			return err
		}
		if _, err := task.OpenShell.Config.call(ctx, "sandbox", "upload", task.OpenShell.Name, path, sandboxInputImages+"/"+image.Name); err != nil {
			return fmt.Errorf("sandbox image upload failed")
		}
	}
	return nil
}

package paddock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Give the transport the selected agent's executable name for terminal process
// discovery. The source is the pinned OpenShell client or Paddock's paste
// transport, never a host coding agent. It is not added to PATH. A
// content-addressed copy survives tool upgrades.
func sandboxAgentProxy(home, binary, agent string) (string, error) {
	if agent != "codex" && agent != "claude" {
		return "", fmt.Errorf("invalid agent proxy name")
	}
	source, e := os.Open(binary)
	if e != nil {
		return "", e
	}
	defer source.Close()
	hash := sha256.New()
	if _, e = io.Copy(hash, source); e != nil {
		return "", e
	}
	dir := filepath.Join(home, "openshell-clients", hex.EncodeToString(hash.Sum(nil)))
	if e = os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	dest := filepath.Join(dir, agent)
	if existing, e := os.Open(dest); e == nil {
		h := sha256.New()
		_, readErr := io.Copy(h, existing)
		existing.Close()
		if readErr != nil || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(hash.Sum(nil)) {
			return "", fmt.Errorf("OpenShell agent proxy integrity mismatch")
		}
		return dest, nil
	} else if !os.IsNotExist(e) {
		return "", e
	}
	// Copy rather than hard-link: modifying either executable must not modify the
	// other. Atomic rename also handles simultaneous task launches safely.
	if _, e = source.Seek(0, io.SeekStart); e != nil {
		return "", e
	}
	tmp, e := os.CreateTemp(dir, ".client-")
	if e != nil {
		return "", e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, e = io.Copy(tmp, source); e != nil {
		return "", e
	}
	if e = tmp.Chmod(0700); e != nil {
		return "", e
	}
	if e = tmp.Sync(); e != nil {
		return "", e
	}
	if e = tmp.Close(); e != nil {
		return "", e
	}
	if e = replaceFile(tmp.Name(), dest); e != nil {
		return "", e
	}
	return dest, nil
}

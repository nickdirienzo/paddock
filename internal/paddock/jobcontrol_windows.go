package paddock

import (
	"os"
	"os/exec"
)

func prepareAgentTerminal(*exec.Cmd) (func() error, bool, error) {
	return func() error { return nil }, false, nil
}
func signalAgent(c *exec.Cmd, _ bool, sig os.Signal) error { return c.Process.Signal(sig) }

package paddock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func clipboardTerminal() bool { return false }
func openClipboardImage(string) (*os.File, error) {
	return nil, fmt.Errorf("native Windows image paste is unsupported")
}
func runPasteTerminal(context.Context, *exec.Cmd, func(context.Context, string) (string, error)) (int, error) {
	return 1, fmt.Errorf("native Windows image paste is unsupported")
}

package paddock

import "fmt"

func validateCodexTUI(mode string) error {
	switch mode {
	case "", "default", "scrollback", "fullscreen":
		return nil
	default:
		return fmt.Errorf("codex-tui must be default, scrollback, or fullscreen")
	}
}

func codexTUIMode(t Task) string {
	if t.Session.CodexTUI != "" {
		return t.Session.CodexTUI
	}
	if t.Launcher != nil {
		return normalizedCodexTUI(t.Launcher.CodexTUI)
	}
	return "default"
}

func normalizedCodexTUI(mode string) string {
	if mode == "" {
		return "default"
	}
	return mode
}

func initialCodexTUIMode(t Task) string {
	if t.Launcher != nil {
		return normalizedCodexTUI(t.Launcher.CodexTUI)
	}
	return codexTUIMode(t)
}

func codexTUIArgs(t Task) []string {
	if t.Session.Agent != "codex" {
		return nil
	}
	switch codexTUIMode(t) {
	case "scrollback":
		return []string{"--no-alt-screen", "--config", "tui.fullscreen_transcript=false"}
	case "fullscreen":
		return []string{"--config", `tui.alternate_screen="always"`, "--config", "tui.fullscreen_transcript=true"}
	default:
		return nil
	}
}

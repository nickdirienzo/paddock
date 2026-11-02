package paddock

import "strings"

// Never propagate App private material to Git, terminal servers, gh, or agents.
// App JWTs/tokens are minted only inside the HTTP transport, not exported at all.
func credentialFreeEnvironment(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case "PADDOCK_GITHUB_PRIVATE_KEY", "PADDOCK_GITHUB_PRIVATE_KEY_FILE", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_PATH", "GITHUB_APP_PRIVATE_KEY_FILE", "GH_APP_PRIVATE_KEY":
			continue
		}
		if strings.Contains(value, "-----BEGIN") && strings.Contains(value, "PRIVATE KEY-----") {
			continue
		}
		if strings.HasPrefix(value, "ghs_") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

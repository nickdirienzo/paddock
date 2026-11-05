package paddock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// This dedicated native client uses Codex's MCP connections and OAuth store.
// It never starts a model turn or exposes the app-server protocol to a workload.
type mcpNative struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	write   sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[int]chan nativeMCPReply
	done    chan struct{}
	thread  string
}

type nativeMCPReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (n *mcpNative) send(value any) error {
	n.write.Lock()
	defer n.write.Unlock()
	return json.NewEncoder(n.input).Encode(value)
}

func (n *mcpNative) read(output io.Reader) {
	defer close(n.done)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
			continue
		}
		if envelope.Method != "" {
			// Never grant host approvals, execute host tools, or answer auth
			// elicitation on behalf of an operator.
			if len(envelope.ID) > 0 {
				_ = n.send(map[string]any{"id": envelope.ID, "error": map[string]any{"code": -32601, "message": "Host interaction is unavailable in the Paddock MCP bridge"}})
			}
			continue
		}
		var reply nativeMCPReply
		if json.Unmarshal(scanner.Bytes(), &reply) != nil {
			continue
		}
		n.mu.Lock()
		waiting := n.pending[reply.ID]
		n.mu.Unlock()
		if waiting != nil {
			select {
			case waiting <- reply:
			default:
			}
		}
	}
}

func (n *mcpNative) call(ctx context.Context, method string, params any, result any) error {
	n.mu.Lock()
	n.next++
	id := n.next
	waiting := make(chan nativeMCPReply, 1)
	n.pending[id] = waiting
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.pending, id); n.mu.Unlock() }()
	if err := n.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("host MCP connection closed")
	}
	select {
	case reply := <-waiting:
		// Native errors can contain authentication headers. Do not forward them.
		if len(reply.Error) > 0 {
			return fmt.Errorf("host MCP request failed (%s); check the host MCP connection and login", method)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("invalid host MCP response (%s)", method)
		}
		return nil
	case <-n.done:
		return fmt.Errorf("host MCP connection closed")
	case <-ctx.Done():
		return fmt.Errorf("host MCP request interrupted (%s); inspect the service before retrying a tool call", method)
	}
}

func nativeMCPEnvironment(set trustedMCPSet) []string {
	keys := map[string]bool{"HOME": true, "PATH": true, "TMPDIR": true, "TEMP": true, "TMP": true, "SYSTEMROOT": true, "USERPROFILE": true, "LOCALAPPDATA": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	for _, name := range set.names {
		server := set.config.Servers[name]
		if key, ok := server["bearer_token_env_var"].(string); ok {
			keys[key] = true
		}
		if headers, ok := server["env_http_headers"].(map[string]any); ok {
			for _, key := range headers {
				if key, ok := key.(string); ok {
					keys[key] = true
				}
			}
		}
	}
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "CODEX_HOME" && keys[key] {
			env = append(env, value)
		}
	}
	return append(env, "CODEX_HOME="+set.home)
}

func startNativeMCP(ctx context.Context, set trustedMCPSet, dir string) (*mcpNative, func(), error) {
	path, err := executable("PADDOCK_CODEX_BIN", "codex")
	if err != nil {
		return nil, nil, fmt.Errorf("trusted MCP bridge requires host Codex: %w", err)
	}
	// An empty table merges with user configuration, rather than deleting it.
	// Explicitly disable every unselected server, including host stdio commands.
	names := make([]string, 0, len(set.config.Servers))
	for name := range set.config.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var entries []string
	for _, name := range names {
		_, selected := set.grants[name]
		entries = append(entries, tomlString(name)+fmt.Sprintf("={enabled=%t}", selected))
	}
	args := []string{"app-server", "--listen", "stdio://", "--config", "mcp_servers={" + strings.Join(entries, ",") + "}"}
	features := map[string]any{}
	for _, feature := range []string{"hooks", "plugins", "apps", "shell_tool", "shell_snapshot", "mcp_connection_sharing", "skill_mcp_dependency_install"} {
		args = append(args, "--config", "features."+feature+"=false")
		features["features."+feature] = false
	}
	args = append(args, "--config", "project_doc_max_bytes=0", "--config", "web_search=\"disabled\"")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = dir, nativeMCPEnvironment(set)
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = input.Close()
		return nil, nil, fmt.Errorf("start host MCP connection: %w", err)
	}
	n := &mcpNative{cmd: cmd, input: input, pending: map[int]chan nativeMCPReply{}, done: make(chan struct{})}
	go n.read(output)
	var once sync.Once
	stop := func() { once.Do(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait(); <-n.done }) }
	fail := func(err error) (*mcpNative, func(), error) { stop(); return nil, nil, err }
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err = n.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "paddock-mcp-bridge", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return fail(err)
	}
	if err = n.send(map[string]string{"method": "initialized"}); err != nil {
		return fail(fmt.Errorf("initialize host MCP connection"))
	}
	// Pin the reviewed definitions through stdin; headers never enter argv.
	servers := map[string]any{}
	for name, server := range set.config.Servers {
		if _, selected := set.grants[name]; selected {
			servers[name] = server
		} else {
			servers[name] = map[string]bool{"enabled": false}
		}
	}
	features["mcp_servers"] = servers
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = n.call(ctx, "thread/start", map[string]any{"ephemeral": true, "cwd": dir, "sandbox": "read-only", "approvalPolicy": "never", "config": features}, &started); err != nil {
		return fail(err)
	}
	if started.Thread.ID == "" {
		return fail(fmt.Errorf("host MCP connection returned no thread identity"))
	}
	n.thread = started.Thread.ID
	return n, stop, nil
}

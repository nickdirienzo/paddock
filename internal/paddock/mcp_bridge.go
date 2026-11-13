package paddock

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

//go:embed sandbox_mcp.py
var sandboxMCPClient string

const installSandboxMCP = `import json,pathlib,sys,tomllib
root=pathlib.Path('/sandbox/.paddock')
root.mkdir(parents=True,exist_ok=True,mode=0o700)
payload=json.loads(sys.argv[1])
begin='# BEGIN PADDOCK TRUSTED MCPS\n'
end='# END PADDOCK TRUSTED MCPS\n'
config=pathlib.Path('/home/sandbox/.codex/config.toml')
text=config.read_text() if config.exists() else ''
if begin in text:
    before,block=text.split(begin,1)
    if end not in block: raise RuntimeError('Incomplete Paddock MCP configuration block')
    text=before+block.split(end,1)[1]
parsed=tomllib.loads(text)
for name in payload['names']:
    if name in parsed.get('mcp_servers',{}):
        raise RuntimeError('An unmanaged MCP configuration already uses a trusted server name')
text=text.rstrip()+'\n\n'+begin+payload['codex']+end
tomllib.loads(text)
codex=config
claude=pathlib.Path('/home/sandbox/.claude/.claude.json')
data=json.loads(claude.read_text()) if claude.exists() else {}
managed=root/'mcp-managed.json'
old=json.loads(managed.read_text()) if managed.exists() else []
servers=data.setdefault('mcpServers',{})
for name in old: servers.pop(name,None)
for name in payload['names']:
    if name in servers: raise RuntimeError('An unmanaged Claude MCP already uses a trusted server name')
servers.update(payload['claude'])
for config,content in ((codex,text),(claude,json.dumps(data))):
    config.parent.mkdir(parents=True,exist_ok=True)
    temp=config.with_name(config.name+'.paddock-tmp')
    temp.write_text(content); temp.chmod(0o600); temp.replace(config)
managed.write_text(json.dumps(payload['names']))
script=root/'mcp.py'
script.write_text(sys.argv[2]); script.chmod(0o700)
`

func installTrustedMCPs(ctx context.Context, task Task, names []string) error {
	payload, err := sandboxMCPConfiguration(names)
	if err != nil {
		return err
	}
	_, err = sandboxOutput(ctx, task, "/usr/bin/python3", "-c", installSandboxMCP, string(payload), sandboxMCPClient)
	return err
}

func sandboxMCPConfiguration(names []string) ([]byte, error) {
	codex, claude := map[string]any{}, map[string]any{}
	for _, name := range names {
		args := []string{"/sandbox/.paddock/mcp.py", "client", "/sandbox/.paddock", name}
		codex[name] = map[string]any{"command": "/usr/bin/python3", "args": args, "startup_timeout_sec": 120, "tool_timeout_sec": 600}
		claude[name] = map[string]any{"type": "stdio", "command": "/usr/bin/python3", "args": args}
	}
	var fragment []byte
	var err error
	if len(names) > 0 {
		fragment, err = toml.Marshal(map[string]any{"mcp_servers": codex})
	}
	if err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return json.Marshal(map[string]any{"names": names, "codex": string(fragment), "claude": claude})
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpInventory struct {
	Name              string                     `json:"name"`
	Tools             map[string]json.RawMessage `json:"tools"`
	ToolsError        *string                    `json:"toolsError"`
	Resources         []json.RawMessage          `json:"resources"`
	ResourceTemplates []json.RawMessage          `json:"resourceTemplates"`
}

type mcpCaller interface {
	call(context.Context, string, any, any) error
}

type mcpBroker struct {
	storeHome, commonDir string
	set                  trustedMCPSet
	native               mcpCaller
	thread               string
}

func (b mcpBroker) toolAllowed(server, tool string) bool {
	config := b.set.config.Servers[server]
	for _, key := range []string{"enabled_tools", "disabled_tools"} {
		value, present := config[key]
		if !present {
			continue
		}
		data, err := json.Marshal(value)
		var names []string
		if err != nil || json.Unmarshal(data, &names) != nil {
			return false
		}
		found := false
		for _, name := range names {
			if name == tool {
				found = true
				break
			}
		}
		if (key == "enabled_tools" && !found) || (key == "disabled_tools" && found) {
			return false
		}
	}
	return true
}

func (b mcpBroker) inventory(ctx context.Context, name string) (mcpInventory, error) {
	var response struct {
		Data       []mcpInventory `json:"data"`
		NextCursor *string        `json:"nextCursor"`
	}
	params := map[string]any{"serverName": name, "threadId": b.thread}
	for {
		if err := b.native.call(ctx, "mcpServerStatus/list", params, &response); err != nil {
			return mcpInventory{}, err
		}
		for _, server := range response.Data {
			if server.Name == name {
				return server, nil
			}
		}
		if response.NextCursor == nil {
			return mcpInventory{}, fmt.Errorf("trusted MCP is unavailable; check its host login")
		}
		params["cursor"] = *response.NextCursor
	}
}

// readyTrustedMCPs checks the host catalog before advertising bridge clients to
// an interactive agent. A failed server would otherwise stop Claude at its MCP
// startup menu even when other servers are usable.
func readyTrustedMCPs(ctx context.Context, broker mcpBroker, names []string) (ready, unavailable []string) {
	for _, name := range names {
		check, cancel := context.WithTimeout(ctx, 20*time.Second)
		inventory, err := broker.inventory(check, name)
		cancel()
		if err != nil || inventory.ToolsError != nil ||
			(inventory.Tools == nil && inventory.Resources == nil && inventory.ResourceTemplates == nil) {
			unavailable = append(unavailable, name)
			continue
		}
		ready = append(ready, name)
	}
	return ready, unavailable
}

func (b mcpBroker) dispatch(ctx context.Context, name string, request mcpRequest) any {
	errorReply := func(code int, message string) any {
		return map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": code, "message": message}}
	}
	if err := b.set.authorize(b.storeHome, b.commonDir, name); err != nil {
		return errorReply(-32001, "MCP trust is unavailable or changed; review paddock repo trust on the host")
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return errorReply(-32600, "Invalid MCP request")
	}
	var result any
	var err error
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(request.Params, &params) != nil {
			return errorReply(-32602, "Invalid initialization parameters")
		}
		version := params.ProtocolVersion
		switch version {
		case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		default:
			version = "2025-06-18"
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]string{"name": "paddock-" + name, "version": "1"}}
	case "notifications/initialized", "notifications/cancelled":
		return nil
	case "ping":
		result = map[string]any{}
	case "tools/list", "resources/list", "resources/templates/list":
		var inventory mcpInventory
		inventory, err = b.inventory(ctx, name)
		if err != nil {
			break
		}
		switch request.Method {
		case "tools/list":
			if inventory.ToolsError != nil {
				err = fmt.Errorf("tool discovery failed; check the trusted MCP's host connection and login")
				break
			}
			names := make([]string, 0, len(inventory.Tools))
			for key := range inventory.Tools {
				names = append(names, key)
			}
			sort.Strings(names)
			tools := []json.RawMessage{}
			for _, key := range names {
				var tool struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(inventory.Tools[key], &tool) == nil && b.toolAllowed(name, tool.Name) {
					tools = append(tools, inventory.Tools[key])
				}
			}
			result = map[string]any{"tools": tools}
		case "resources/list":
			if inventory.Resources == nil {
				inventory.Resources = []json.RawMessage{}
			}
			result = map[string]any{"resources": inventory.Resources}
		case "resources/templates/list":
			if inventory.ResourceTemplates == nil {
				inventory.ResourceTemplates = []json.RawMessage{}
			}
			result = map[string]any{"resourceTemplates": inventory.ResourceTemplates}
		}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal(request.Params, &params) != nil || params.Name == "" {
			return errorReply(-32602, "Invalid tool call")
		}
		var inventory mcpInventory
		inventory, err = b.inventory(ctx, name)
		if err != nil {
			break
		}
		allowed := false
		for _, raw := range inventory.Tools {
			var tool struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &tool) == nil && tool.Name == params.Name {
				allowed = true
				break
			}
		}
		if !allowed || inventory.ToolsError != nil || !b.toolAllowed(name, params.Name) {
			return errorReply(-32602, "Tool is unavailable in the trusted MCP catalog")
		}
		call := map[string]any{"threadId": b.thread, "server": name, "tool": params.Name}
		if len(params.Arguments) > 0 {
			call["arguments"] = params.Arguments
		}
		if len(params.Meta) > 0 {
			call["_meta"] = params.Meta
		}
		var raw json.RawMessage
		err = b.native.call(ctx, "mcpServer/tool/call", call, &raw)
		result = raw
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(request.Params, &params) != nil || params.URI == "" {
			return errorReply(-32602, "Invalid resource request")
		}
		var raw json.RawMessage
		err = b.native.call(ctx, "mcpServer/resource/read", map[string]any{"threadId": b.thread, "server": name, "uri": params.URI}, &raw)
		result = raw
	default:
		return errorReply(-32601, "Method is not available through the trusted MCP bridge")
	}
	if err != nil {
		return errorReply(-32000, err.Error())
	}
	return map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
}

// startTrustedMCPBridge owns both sides for exactly one supervisor lifetime.
// All outbound MCP traffic and OAuth refresh remain on the host; the workload
// can address only its repository's explicitly selected MCP server names.
func startTrustedMCPBridge(store *Store, task Task) (func(), []string, error) {
	set, err := loadTrustedMCPs(store.Home, task.CommonDir)
	if err != nil {
		return nil, nil, err
	}
	if len(set.names) == 0 {
		setup, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
		err = installTrustedMCPs(setup, task, nil)
		cancelSetup()
		if err != nil {
			return nil, nil, fmt.Errorf("configure trusted MCPs: %w", err)
		}
		return func() {}, nil, nil
	}
	dir, err := os.MkdirTemp(store.Home, ".mcp-bridge-")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	native, stopNative, err := startNativeMCP(ctx, set, dir)
	if err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	broker := mcpBroker{storeHome: store.Home, commonDir: task.CommonDir, set: set, native: native, thread: native.thread}
	readyNames, unavailable := readyTrustedMCPs(ctx, broker, set.names)
	setup, cancelSetup := context.WithTimeout(ctx, 30*time.Second)
	err = installTrustedMCPs(setup, task, readyNames)
	cancelSetup()
	if err != nil {
		cancel()
		stopNative()
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("configure trusted MCPs: %w", err)
	}
	if len(readyNames) == 0 {
		cancel()
		stopNative()
		_ = os.RemoveAll(dir)
		return func() {}, unavailable, nil
	}
	cmd := sandboxExec(ctx, task, false, "/usr/bin/python3", "-u", "/sandbox/.paddock/mcp.py", "serve", "/sandbox/.paddock")
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		stopNative()
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		cancel()
		stopNative()
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = input.Close()
		cancel()
		stopNative()
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("start sandbox MCP bridge: %w", err)
	}
	var once sync.Once
	var workers sync.WaitGroup
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			cancel()
			_ = input.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			stopNative()
			<-done
			workers.Wait()
			_ = os.RemoveAll(dir)
		})
	}
	ready := make(chan bool, 1)
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		if !scanner.Scan() {
			ready <- false
			return
		}
		var status struct {
			Ready bool `json:"ready"`
		}
		if json.Unmarshal(scanner.Bytes(), &status) != nil || !status.Ready {
			ready <- false
			return
		}
		ready <- true
		var write sync.Mutex
		slots := make(chan struct{}, 32)
		for scanner.Scan() {
			var envelope struct {
				ID      int        `json:"id"`
				Server  string     `json:"server"`
				Request mcpRequest `json:"request"`
			}
			if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
				continue
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Minute)
				defer requestCancel()
				response := broker.dispatch(requestCtx, envelope.Server, envelope.Request)
				if len(envelope.Request.ID) == 0 {
					return
				}
				write.Lock()
				defer write.Unlock()
				_ = json.NewEncoder(input).Encode(map[string]any{"id": envelope.ID, "response": response})
			}()
		}
	}()
	select {
	case ok := <-ready:
		if !ok {
			stop()
			return nil, nil, fmt.Errorf("sandbox MCP bridge did not become ready")
		}
	case <-time.After(30 * time.Second):
		stop()
		return nil, nil, fmt.Errorf("sandbox MCP bridge startup timed out")
	}
	fmt.Fprintf(os.Stderr, "Trusted MCP host bridge active for task %s: %v (MCP calls execute on the host)\n", task.ID, readyNames)
	return stop, unavailable, nil
}

package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These opt-in tests launch real native CLIs with isolated homes and local model
// endpoints. They import no native history: a new conversation must actually
// read the production context file and send its contents in a tool result.
func TestNativeRevivalContextIngestion(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			binary := os.Getenv("PADDOCK_NATIVE_" + strings.ToUpper(provider))
			if binary == "" {
				t.Skip("set PADDOCK_NATIVE_" + strings.ToUpper(provider) + " to exercise installed native CLI")
			}
			registry := openTestStore(t, t.TempDir())
			service := Service{Store: registry}
			task := taskFixture()
			task.Runtime = "host"
			task.Worktree = t.TempDir()
			task.BaseCommit = strings.Repeat("a", 40)
			sourceID := newConversationID()
			contextText := "HISTORICAL ARCHIVED CONVERSATION\nUSER: \"ARCHIVED_USER_8c9271\"\nASSISTANT: \"ARCHIVED_ASSISTANT_b6e012\"\n"
			path, err := service.writeRevivalContext(task.ID, 2, contextText)
			if err != nil {
				t.Fatal(err)
			}
			task.Prompt = "ORIGINAL_ASSIGNMENT_MUST_NOT_REPLAY_f19d"
			task.Session = Session{Agent: provider, ContextFile: path, ContextSHA256: digest([]byte(contextText)), RevivedFrom: sourceID, Generation: 2}
			if err := service.verifyRevivalContext(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			prompt := agentPrompt(task, registry.Home, "/fixture/paddock")
			if strings.Contains(prompt, task.Prompt) || strings.Contains(prompt, "ARCHIVED_USER_8c9271") {
				t.Fatal("fresh prompt replays old assignment or embeds rather than reads context")
			}
			var calls atomic.Int32
			var ingested atomic.Bool
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "count_tokens") {
					io.WriteString(w, `{"input_tokens":1}`)
					return
				}
				if !(strings.HasSuffix(r.URL.Path, "responses") || strings.HasSuffix(r.URL.Path, "messages")) {
					http.NotFound(w, r)
					return
				}
				body, _ := io.ReadAll(r.Body)
				first := calls.Add(1) == 1
				if strings.Contains(string(body), task.Prompt) {
					t.Error("original assignment reached model")
				}
				if !first {
					toolResult := "function_call_output"
					if provider == "claude" {
						toolResult = "tool_result"
					}
					good := strings.Contains(string(body), toolResult) && strings.Contains(string(body), "ARCHIVED_USER_8c9271") && strings.Contains(string(body), "ARCHIVED_ASSISTANT_b6e012")
					if !good {
						t.Error("native tool result omitted archived chat")
					}
					ingested.Store(good)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "codex" {
					var item map[string]any
					if first {
						args, _ := json.Marshal(map[string]string{"cmd": "cat -- " + quote(path)})
						item = map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": "exec_command", "arguments": string(args), "status": "completed"}
					} else {
						item = map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Context recovered; waiting for your next instruction.", "annotations": []any{}}}}
					}
					for _, event := range []map[string]any{
						{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress", "output": []any{}}},
						{"type": "response.output_item.added", "output_index": 0, "item": item},
						{"type": "response.output_item.done", "output_index": 0, "item": item},
						{"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
					} {
						data, _ := json.Marshal(event)
						fmt.Fprintf(w, "data: %s\n\n", data)
					}
				} else {
					event := func(kind string, fields map[string]any) {
						fields["type"] = kind
						data, _ := json.Marshal(fields)
						fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
					}
					event("message_start", map[string]any{"message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-opus-4-8", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
					stop := "end_turn"
					if first {
						stop = "tool_use"
						event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": "tool_fixture", "name": "Read", "input": map[string]any{}}})
						input, _ := json.Marshal(map[string]string{"file_path": path})
						event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)}})
					} else {
						event("content_block_start", map[string]any{"index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
						event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "text_delta", "text": "Context recovered; waiting for your next instruction."}})
					}
					event("content_block_stop", map[string]any{"index": 0})
					event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
					event("message_stop", map[string]any{})
				}
			}))
			defer model.Close()
			home := t.TempDir()
			var args, environment []string
			environment = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TERM=dumb"}
			if provider == "codex" {
				config := fmt.Sprintf("model=\"fixture\"\nmodel_provider=\"fixture\"\n[model_providers.fixture]\nname=\"fixture\"\nbase_url=%q\nwire_api=\"responses\"\nrequires_openai_auth=false\n", model.URL+"/v1")
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				environment = append(environment, "CODEX_HOME="+home)
				args = []string{"exec", "--sandbox", "danger-full-access", "--json", "--skip-git-repo-check", prompt}
			} else {
				environment = append(environment, "CLAUDE_CONFIG_DIR="+home, "ANTHROPIC_API_KEY=paddock-fixture", "ANTHROPIC_BASE_URL="+model.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
				args = []string{"--bare", "--print", "--tools", "Read", "--allowedTools", "Read", "--model", "opus", "--output-format", "json", prompt}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, args...)
			command.Dir = task.Worktree
			command.Env = environment
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("native %s: %v: %s", provider, err, output)
			}
			var newID string
			if provider == "codex" {
				for _, line := range strings.Split(string(output), "\n") {
					var e struct {
						Type string `json:"type"`
						ID   string `json:"thread_id"`
					}
					if json.Unmarshal([]byte(line), &e) == nil && e.Type == "thread.started" {
						newID = e.ID
					}
				}
			} else {
				var result struct {
					ID    string `json:"session_id"`
					Error bool   `json:"is_error"`
				}
				if err := json.Unmarshal(output, &result); err != nil || result.Error {
					t.Fatalf("invalid native result: %v: %s", err, output)
				}
				newID = result.ID
			}
			if !conversationUUID.MatchString(newID) || newID == sourceID {
				t.Fatalf("expected a new native conversation, got %q; output %s", newID, output)
			}
			if !ingested.Load() || calls.Load() < 2 {
				t.Fatalf("actual context ingestion not demonstrated: %s", output)
			}
			t.Logf("fresh %s conversation %s read archived user/assistant context; prior conversation %s was not imported", provider, newID, sourceID)
		})
	}
}

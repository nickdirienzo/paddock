package paddock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IdleState is a checkpoint, not permission to kill a process. The runtime must
// accept the exact generation/turn/epoch behind its native admission fence.
type IdleState struct {
	Generation     int    `json:"generation"`
	Phase          string `json:"phase"` // waiting, unsupported, idle, releasing, draining, expired
	TurnID         string `json:"turn_id,omitempty"`
	Epoch          string `json:"epoch,omitempty"`
	Since          string `json:"since,omitempty"`
	Deadline       string `json:"deadline,omitempty"`
	Error          string `json:"error,omitempty"`
	PaneClosed     bool   `json:"pane_closed,omitempty"`
	DrainConfirmed bool   `json:"drain_confirmed,omitempty"`
}

func NormalizeIdleTTL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return "", fmt.Errorf("--idle-ttl must be a positive duration, e.g. 24h (omit for no TTL)")
	}
	return duration.String(), nil
}

func codexControlEndpoint(t Task) string {
	if t.IdleTTL != "" {
		return "unix://" + agentControlDir(t) + "/idle-rpc.sock"
	}
	return "unix:///home/sandbox/.codex/app-server-control/app-server-control.sock"
}

type idleRuntimeState struct {
	State  string `json:"state"`
	TurnID string `json:"turn_id"`
	Epoch  string `json:"epoch"`
	Error  string `json:"error,omitempty"`
}

type idleChecks struct {
	runtime func(context.Context, Task, string) (idleRuntimeState, error)
	output  func(context.Context, Task) ([]OutputEvent, error)
	finish  func(context.Context, Task) error
	now     func() time.Time
	access  func(context.Context, Task) error
}

func idleRuntime(ctx context.Context, t Task, operation string) (idleRuntimeState, error) {
	var result idleRuntimeState
	request := map[string]any{"op": operation, "generation": t.Session.Generation,
		"thread": t.Session.ConversationID, "cwd": t.Worktree}
	if t.Idle != nil {
		request["epoch"], request["turn_id"] = t.Idle.Epoch, t.Idle.TurnID
	}
	data, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	cmd := sandboxExec(ctx, t, false, "/usr/local/bin/paddock-idle", operation)
	cmd.Stdin = strings.NewReader(string(data))
	output, err := cmd.Output()
	if err != nil {
		return result, fmt.Errorf("idle runtime unavailable: %w", err)
	}
	if err = json.Unmarshal(output, &result); err != nil {
		return result, fmt.Errorf("invalid idle runtime response")
	}
	return result, nil
}

// ReconcileIdle is called by the existing runtime poller. It does not adopt
// existing sessions: only explicitly opted-in launches have an admission fence.
func (s Service) ReconcileIdle(ctx context.Context) error {
	return s.reconcileIdle(ctx, time.Now().UTC(), idleChecks{
		runtime: idleRuntime, output: codexOutput, finish: s.finishIdle, now: time.Now,
		access: func(ctx context.Context, t Task) error { _, err := s.SyncAccess(ctx, t); return err },
	})
}

func (s Service) reconcileIdle(ctx context.Context, clock time.Time, checks idleChecks) error {
	tasks, err := s.Store.List("")
	if err != nil {
		return err
	}
	var first error
	for _, t := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.IdleTTL == "" || t.Session.Generation == 0 {
			continue
		}
		release, err := s.Store.taskLease(t.ID, "operation")
		if err != nil {
			continue
		} // delivery/resume wins; retry next pass
		err = func() error {
			t, err := s.Store.Get(t.ID)
			if err != nil {
				return err
			}
			ttl, err := time.ParseDuration(t.IdleTTL)
			if err != nil || ttl <= 0 {
				return fmt.Errorf("invalid saved idle TTL")
			}
			if t.Idle != nil && t.Idle.Generation == t.Session.Generation && t.Idle.Phase == "expired" {
				if !t.Idle.PaneClosed {
					return checks.finish(ctx, t)
				}
				return nil
			}
			if t.Status == "preparing" || t.Status == "starting" || t.Session.ConversationID == "" {
				return nil
			}
			state := IdleState{Generation: t.Session.Generation, Phase: "waiting"}
			if t.RuntimeName() != "openshell" || t.Session.Agent != "codex" {
				state.Phase, state.Error = "unsupported", "no verified native idle release for this runtime/agent; session retained"
				return s.saveIdle(t, state)
			}
			// A durable drain intent is reconciled from the sandbox exit marker even
			// after the host supervisor, gateway connection, or poller has disappeared.
			if t.Idle != nil && t.Idle.Generation == t.Session.Generation && (t.Idle.Phase == "releasing" || t.Idle.Phase == "draining") {
				return checks.finish(ctx, t)
			}
			if t.Status != "running" && t.Status != "interrupted" {
				return nil
			}
			if checks.access != nil {
				if err := checks.access(ctx, t); err != nil {
					state.Error = "live access state is unknown: " + err.Error()
					return s.saveIdle(t, state)
				}
			}
			if err := s.noPendingWork(t.ID); err != nil {
				state.Error = err.Error()
				return s.saveIdle(t, state)
			}
			if err := s.noPendingMessages(t.ID); err != nil {
				state.Error = err.Error()
				return s.saveIdle(t, state)
			}
			native, err := checks.runtime(ctx, t, "status")
			if err != nil || native.State != "completed" || native.TurnID == "" || native.Epoch == "" {
				state.Error = "native work active, blocked, or unknown; session retained"
				if err != nil {
					state.Error = err.Error()
				} else if native.Error != "" {
					state.Error = native.Error
				}
				// A lost expire response can leave a live runtime already draining.
				if native.State == "draining" && t.Idle != nil && t.Idle.Epoch == native.Epoch {
					state = *t.Idle
					state.Phase = "draining"
				}
				return s.saveIdle(t, state)
			}
			state.Phase, state.TurnID, state.Epoch = "idle", native.TurnID, native.Epoch
			// Sample after native confirmation, not at the start of a potentially
			// slow multi-task pass. A turn may have completed during inspection.
			confirmedAt := clock
			if checks.now != nil {
				confirmedAt = checks.now().UTC()
			}
			if t.Idle != nil && t.Idle.Generation == t.Session.Generation && t.Idle.Phase == "idle" &&
				t.Idle.TurnID == native.TurnID && t.Idle.Epoch == native.Epoch && t.Idle.Since != "" {
				state.Since, state.Deadline = t.Idle.Since, t.Idle.Deadline
			} else {
				state.Since, state.Deadline = confirmedAt.Format(time.RFC3339Nano), confirmedAt.Add(ttl).Format(time.RFC3339Nano)
			}
			deadline, err := time.Parse(time.RFC3339Nano, state.Deadline)
			if err != nil {
				return fmt.Errorf("invalid idle deadline")
			}
			if err := s.saveIdle(t, state); err != nil {
				return err
			}
			if confirmedAt.Before(deadline) {
				return nil
			}
			t.Idle = &state
			// Preserve public output before requesting release. A changed runtime epoch
			// rejects the release and this tentative snapshot is never served.
			events, err := checks.output(ctx, t)
			if err != nil {
				state.Error = "cannot checkpoint output: " + err.Error()
				return s.saveIdle(t, state)
			}
			if err = s.saveIdleOutput(t, events); err != nil {
				return err
			}
			if checks.access != nil {
				if err := checks.access(ctx, t); err != nil {
					state.Phase, state.Error = "waiting", "live access state is unknown: "+err.Error()
					return s.saveIdle(t, state)
				}
			}
			if err = s.noPendingWork(t.ID); err != nil {
				state.Phase, state.Error = "waiting", err.Error()
				return s.saveIdle(t, state)
			}
			if err = s.noPendingMessages(t.ID); err != nil {
				state.Phase, state.Error = "waiting", err.Error()
				return s.saveIdle(t, state)
			}
			// Intent is durable BEFORE sending: a crash must reconcile, not blindly send
			// a second signal or assume the owner was safely released.
			state.Phase = "releasing"
			if err = s.saveIdle(t, state); err != nil {
				return err
			}
			t.Idle = &state
			native, err = checks.runtime(ctx, t, "expire")
			if err != nil {
				return err
			} // keep uncertain intent for recovery
			if native.State != "draining" {
				state.Phase, state.Since, state.Deadline, state.Error = "waiting", "", "", "idle release rejected; activity changed or runtime unknown"
				if native.Error != "" {
					state.Error = native.Error
				}
				return s.saveIdle(t, state)
			}
			state.Phase = "draining"
			return s.saveIdle(t, state)
		}()
		if err != nil {
			// Keep cleanup/recovery failures visible to JSON callers as well as
			// the poller log, without overwriting a newer launch's checkpoint.
			_ = s.Store.update(t.ID, func(current *Task) error {
				if current.Session.Generation == t.Session.Generation && current.Idle != nil {
					current.Idle.Error = err.Error()
				}
				return nil
			})
		}
		release()
		if err != nil && first == nil {
			first = fmt.Errorf("task %s idle lifecycle: %w", t.ID, err)
		}
	}
	return first
}

func (s Service) saveIdle(t Task, state IdleState) error {
	return s.Store.update(t.ID, func(current *Task) error {
		if current.Session.Generation != t.Session.Generation {
			return fmt.Errorf("stale idle generation")
		}
		current.Idle = &state
		return nil
	})
}

func (s Service) noPendingMessages(id string) error {
	files, err := filepath.Glob(filepath.Join(s.Store.Home, "messages", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var receipt MessageReceipt
		if err = json.Unmarshal(data, &receipt); err != nil {
			return err
		}
		if receipt.TaskID == id && receipt.Status != "delivered" {
			return fmt.Errorf("pending or uncertain message delivery")
		}
	}
	return nil
}

// verifiedIdleExit checks the runtime's proof of graceful owner exit. A saved
// history entry, successful transport exit, or stopped sandbox alone never proves it.
func verifiedIdleExit(ctx context.Context, t Task) bool {
	if t.Idle == nil || t.Idle.Generation != t.Session.Generation ||
		(t.Idle.Phase != "releasing" && t.Idle.Phase != "draining" && t.Idle.Phase != "expired") {
		return false
	}
	output, err := sandboxOutput(ctx, t, "cat", agentControlDir(t)+"/idle-stopped.json")
	var marker struct {
		Generation int    `json:"generation"`
		Thread     string `json:"thread"`
		TurnID     string `json:"turn_id"`
		Epoch      string `json:"epoch"`
	}
	return err == nil && json.Unmarshal([]byte(output), &marker) == nil && marker.Generation == t.Session.Generation &&
		marker.Thread == t.Session.ConversationID && marker.TurnID == t.Idle.TurnID && marker.Epoch == t.Idle.Epoch
}

func (s Service) finishIdle(ctx context.Context, t Task) error {
	if !t.Idle.DrainConfirmed {
		if !verifiedIdleExit(ctx, t) {
			native, err := idleRuntime(ctx, t, "status")
			if err == nil && native.State == "completed" {
				// The durable intent never reached the runtime, or was rejected before a
				// response could be saved. Start a new countdown; do not resend shutdown.
				return s.saveIdle(t, IdleState{Generation: t.Session.Generation, Phase: "waiting"})
			}
			if err == nil && native.State == "draining" && native.Epoch == t.Idle.Epoch {
				state := *t.Idle
				state.Phase = "draining"
				return s.saveIdle(t, state)
			}
			return fmt.Errorf("native idle drain exit is not yet confirmed; retained")
		}
		t.Idle.DrainConfirmed, t.Idle.Phase = true, "draining"
		if err := s.saveIdle(t, *t.Idle); err != nil {
			return err
		}
	}
	release, err := s.Store.taskLease(t.ID, "process")
	if err != nil {
		return nil
	} // let the live supervisor record its exit first
	defer release()
	if t.Session.Backend == "rex" && (t.Session.ID == "" || t.Session.BlockID == "") {
		return fmt.Errorf("owned Rex pane was not recorded; refusing to infer ownership")
	}
	state, err := inspectTaskSandbox(ctx, t)
	if err != nil {
		return err
	}
	if state.Phase == "Ready" {
		if err := stopCompletedSandbox(ctx, t); err != nil {
			return err
		}
	} else if state.Phase != "Stopped" && state.Phase != "Completed" {
		return fmt.Errorf("sandbox shutdown state is unknown; retained")
	}
	if err := closeExitedTerminal(ctx, t); err != nil {
		return err
	}
	return s.Store.update(t.ID, func(current *Task) error {
		if current.Session.Generation != t.Session.Generation {
			return fmt.Errorf("stale idle cleanup")
		}
		code := 0
		if current.Status != "stopped" {
			current.Session.EndedAt, current.LastActivityAt = now(), now()
		}
		current.Status, current.Error, current.Session.ExitCode = "stopped", "", &code
		current.Session.LastTurn = &AgentTurn{ID: t.Idle.TurnID, State: "completed"}
		current.Idle.Phase, current.Idle.PaneClosed, current.Idle.Error = "expired", true, ""
		return nil
	})
}

type idleOutputSnapshot struct {
	Conversation string        `json:"conversation"`
	Generation   int           `json:"generation"`
	Epoch        string        `json:"epoch"`
	Events       []OutputEvent `json:"events"`
}

func (s Service) saveIdleOutput(t Task, events []OutputEvent) error {
	return writeJSONAtomic(filepath.Join(s.Store.Home, "tasks", t.ID+".output"), idleOutputSnapshot{
		Conversation: t.Session.ConversationID, Generation: t.Session.Generation, Epoch: t.Idle.Epoch, Events: events})
}

// Output serves an accepted expiry checkpoint without starting the sandbox.
func (s Service) Output(ctx context.Context, t Task, after string, limit int) (OutputPage, error) {
	if t.Idle != nil && (t.Idle.Phase == "draining" || t.Idle.Phase == "expired") {
		data, err := os.ReadFile(filepath.Join(s.Store.Home, "tasks", t.ID+".output"))
		var snapshot idleOutputSnapshot
		if err == nil && json.Unmarshal(data, &snapshot) == nil && snapshot.Conversation == t.Session.ConversationID &&
			snapshot.Generation == t.Session.Generation && snapshot.Epoch == t.Idle.Epoch {
			return pageOutput(t, snapshot.Events, after, limit)
		}
	}
	return NativeOutput(ctx, t, after, limit)
}

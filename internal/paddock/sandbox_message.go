package paddock

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const sandboxMessageLimit = 64 << 10

type sandboxMessageRequest struct {
	ID         string `json:"id"`
	Target     string `json:"target"`
	Body       string `json:"body"`
	ExternalID string `json:"external_id"`
}

type sandboxMessageResult struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id,omitempty"`
	ExternalID  string `json:"external_id"`
	Status      string `json:"status"`
	ThreadID    string `json:"thread_id,omitempty"`
	Error       string `json:"error,omitempty"`
	ContentHash string `json:"content_hash"`
}

// These helpers run inside the source sandbox. The host supplies the sandbox
// identity and resolves its parent from the registry; no host path or command
// is accepted from a request.
const readSandboxMessageRequest = `import json,os,pathlib,re,stat,sys
root=pathlib.Path('/sandbox/.paddock/message-requests')
try:
    names=sorted(os.listdir(root))
except FileNotFoundError:
    sys.exit(0)
for name in names:
    if not re.fullmatch('[0-9a-f]{24}',name): continue
    try:
        fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        with os.fdopen(fd,'rb') as f:
            if not stat.S_ISREG(os.fstat(f.fileno()).st_mode): continue
            data=f.read(524289)
        if len(data)>524288: continue
        request=json.loads(data)
        if not isinstance(request,dict) or request.get('id')!=name: continue
        if any(not isinstance(request.get(key,''),str) for key in ('target','body','external_id')): continue
        print(json.dumps(request)); break
    except (OSError,ValueError,UnicodeError): continue
`

const acknowledgeSandboxMessage = `import os,pathlib,secrets,sys
root=pathlib.Path('/sandbox/.paddock')
temp=root/('.message-response-'+secrets.token_hex(12))
fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'w') as f: f.write(sys.argv[2])
os.replace(temp,root/('message-response-'+sys.argv[1]))
if sys.argv[3]!='pending':
    (root/'message-requests'/sys.argv[1]).unlink(missing_ok=True)
`

func (s Service) deliverSandboxMessage(ctx context.Context, source Task, request sandboxMessageRequest, deliver func(context.Context, Task, string) (string, error)) sandboxMessageResult {
	result := sandboxMessageResult{ID: request.ID, ExternalID: request.ExternalID, Status: "rejected"}
	result.ContentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(request.Target+"\x00"+request.Body)))
	if !validID(request.ID) || request.ExternalID == "" || len(request.ExternalID) > 256 ||
		strings.TrimSpace(request.Body) == "" || len(request.Body) > sandboxMessageLimit || strings.ContainsRune(request.Body, '\x00') {
		result.Error = "invalid sandbox message (body limit: 65536 bytes)"
		return result
	}
	// Re-read the host-owned record. A sandbox cannot choose its sender or
	// redirect delivery by setting PADDOCK_TASK_ID or a parent environment variable.
	current, err := s.Store.Get(source.ID)
	if err != nil || current.RuntimeName() != "openshell" || current.OpenShell == nil || !current.OpenShell.Ready ||
		current.ParentID == "" || request.Target != current.ParentID {
		result.Error = "sandbox messages may target only this task's registered parent (use its full task ID)"
		return result
	}
	parent, err := s.Store.Get(current.ParentID)
	if err != nil {
		result.Error = "registered parent task is unavailable"
		return result
	}
	result.TaskID = parent.ID
	body := "Paddock message from child task " + current.ID + ":\n\n" + request.Body
	receipt, claimed, err := s.Store.ClaimMessage(parent.ID, "sandbox:"+current.ID, request.ExternalID, body)
	if err != nil {
		result.Error = err.Error()
		if errors.Is(err, ErrAgentOffline) {
			result.Status = "pending"
		}
		return result
	}
	if claimed {
		thread, deliveryErr := deliver(ctx, parent, body)
		status, message := "delivered", ""
		if deliveryErr != nil {
			status, message = "uncertain", deliveryErr.Error()
			if errors.Is(deliveryErr, ErrAgentOffline) {
				status = "pending"
			}
		}
		receipt, err = s.Store.FinishMessage(receipt, status, thread, message)
		if err != nil {
			result.Status, result.Error = "uncertain", "could not save delivery receipt; inspect the parent before retrying"
			return result
		}
	}
	result.Status, result.ThreadID, result.Error = receipt.Status, receipt.ThreadID, receipt.Error
	if result.Status == "claimed" {
		result.Status, result.Error = "uncertain", "delivery was interrupted; inspect the parent before retrying"
	}
	return result
}

func (s Service) pollSandboxMessage(ctx context.Context, source Task) error {
	data, err := sandboxOutput(ctx, source, "python3", "-c", readSandboxMessageRequest)
	if err != nil || data == "" {
		return err
	}
	var request sandboxMessageRequest
	if err := json.Unmarshal([]byte(data), &request); err != nil {
		return err
	}
	if !validID(request.ID) {
		return fmt.Errorf("invalid sandbox message request ID")
	}
	result := s.deliverSandboxMessage(ctx, source, request, s.Message)
	response, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = sandboxOutput(ctx, source, "python3", "-c", acknowledgeSandboxMessage, request.ID, string(response), result.Status)
	return err
}

// RunSandboxMessages shares the runtime singleton, but has its own loop so
// native message delivery cannot stall host-shell requests or token renewal.
func (s Service) RunSandboxMessages(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		tasks, err := s.Store.List("")
		if err == nil {
			for _, source := range tasks {
				if source.RuntimeName() != "openshell" || source.OpenShell == nil || !source.OpenShell.Ready || source.Status != "running" || source.ParentID == "" {
					continue
				}
				pass, cancel := context.WithTimeout(ctx, 45*time.Second)
				if err := s.pollSandboxMessage(pass, source); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "Sandbox message %s: %v\n", source.ID, err)
				}
				cancel()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

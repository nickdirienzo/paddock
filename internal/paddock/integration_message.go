package paddock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type MessageReceipt struct {
	Version      int    `json:"version"`
	TaskID       string `json:"task_id"`
	ExternalID   string `json:"external_id"`
	Caller       string `json:"caller"`
	Body         string `json:"body"`
	DeliveryBody string `json:"delivery_body,omitempty"`
	Status       string `json:"status"` // claimed, pending, delivered, uncertain
	ThreadID     string `json:"thread_id,omitempty"`
	Error        string `json:"error,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

// NativeBody returns the exact body frozen when the message was admitted.
// Receipts from older versions contain only Body and retain that delivery.
func (r MessageReceipt) NativeBody() string {
	if r.DeliveryBody != "" {
		return r.DeliveryBody
	}
	return r.Body
}

func (s *Store) messageReceiptPath(taskID, caller, externalID string) string {
	return filepath.Join(s.Home, "messages", githubID(taskID+"\x00"+caller+"\x00"+externalID)+".json")
}

// ClaimMessage persists intent before native queue delivery. A crash after
// claim is uncertain and must never be replayed automatically.
func (s *Store) ClaimMessage(taskID, caller, externalID, body string) (MessageReceipt, bool, error) {
	return s.claimMessage(taskID, caller, externalID, body, false)
}

// ClaimAuthoredMessage admits integration/local user input, freezing runtime
// guidance before native delivery. Body remains the authored idempotency input;
// retries preserve the stored native payload across task or binary upgrades.
func (s *Store) ClaimAuthoredMessage(taskID, caller, externalID, body string) (MessageReceipt, bool, error) {
	return s.claimMessage(taskID, caller, externalID, body, true)
}

func (s *Store) claimMessage(taskID, caller, externalID, body string, authored bool) (MessageReceipt, bool, error) {
	if externalID == "" || len(externalID) > 256 || caller == "" {
		return MessageReceipt{}, false, fmt.Errorf("invalid message idempotency key")
	}
	release, err := s.taskLease(taskID, "operation")
	if err != nil {
		return MessageReceipt{}, false, fmt.Errorf("%w: %v", ErrAgentOffline, err)
	}
	defer release()
	var receipt MessageReceipt
	claimed := false
	err = s.withLock(func() error {
		task, err := s.read(taskID)
		if err != nil {
			return err
		}
		if caller == accessNotificationCaller {
			// A stale poller may still hold a pending copy after the access
			// outbox was terminally disposed. Check its durable admission while
			// holding the same operation/registry locks as every message claim.
			state, err := s.readAccessState(taskID)
			if err != nil {
				return err
			}
			d := state.Decisions[strings.TrimPrefix(externalID, "access:")]
			if d == nil || !d.BackendAcknowledged || d.NotificationBody != body || d.Receipt.Notification.ExternalID != externalID {
				return fmt.Errorf("invalid access notification identity")
			}
			if d.Receipt.Notification.Status != "pending" {
				return nil
			}
		}
		path := s.messageReceiptPath(taskID, caller, externalID)
		data, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(data, &receipt); err != nil {
				return err
			}
			if receipt.Version != 1 || receipt.TaskID != taskID || receipt.Caller != caller || receipt.ExternalID != externalID || receipt.Body != body {
				return fmt.Errorf("message idempotency key belongs to different content")
			}
			if receipt.Status != "pending" {
				return nil
			}
		} else if os.IsNotExist(err) {
			receipt = MessageReceipt{Version: 1, TaskID: taskID, Caller: caller, ExternalID: externalID, Body: body}
			if authored {
				delivery := AuthoredMessageBody(task, body)
				if delivery != body {
					receipt.DeliveryBody = delivery
				}
			}
		} else {
			return err
		}
		receipt.Status, receipt.Error, receipt.UpdatedAt = "claimed", "", now()
		if err := writeJSONAtomic(path, receipt); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return receipt, claimed, err
}

func (s *Store) FinishMessage(receipt MessageReceipt, status, thread, message string) (MessageReceipt, error) {
	if status != "pending" && status != "delivered" && status != "uncertain" {
		return receipt, fmt.Errorf("invalid message status")
	}
	err := s.withLock(func() error {
		path := s.messageReceiptPath(receipt.TaskID, receipt.Caller, receipt.ExternalID)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var current MessageReceipt
		if err := json.Unmarshal(data, &current); err != nil {
			return err
		}
		if current != receipt || current.Status != "claimed" {
			return fmt.Errorf("message claim changed")
		}
		current.Status, current.ThreadID, current.Error, current.UpdatedAt = status, thread, message, now()
		if err := writeJSONAtomic(path, current); err != nil {
			return err
		}
		receipt = current
		return nil
	})
	return receipt, err
}

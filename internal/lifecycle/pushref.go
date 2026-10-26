package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"
)

// ConfigRef names a push config wherever it crosses Temporal history: the kontra.push signal,
// list_push_configs' result and push_webhook's input (spec §10.2, §14.1, §14.2). A config's id may be chosen
// by the caller (A2A lets a client name its configs, up to 128 characters), and no caller-authored value
// crosses history but a review-window message's caller_metadata keys (spec §14), so history carries this
// digest of the task id and the config id instead, and push_webhook finds the config among the task's (at
// most 10) by it. It is stable: a config deleted and set again with the same id has the same ref.
func ConfigRef(taskID uuid.UUID, configID string) string {
	h := sha256.New()
	h.Write([]byte("kontra.push_config\x00"))
	h.Write(taskID[:])
	h.Write([]byte(configID))
	return "pc_" + hex.EncodeToString(h.Sum(nil))
}

// Stopped values of PushResult: why a push_webhook ended.
const (
	PushStoppedTerminal = "terminal" // the terminal snapshot is delivered
	PushStoppedDeleted  = "deleted"  // the config no longer exists
	PushStoppedFailed   = "failed"   // the config is marked failed
)

package gatemcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/auth"
)

// PushConfigs is the store of the A2A push configs (*push.ConfigStore, spec §10.1), which the push tools of /mcp set,
// list and delete for the principal's hire tasks (D337): the same rows, limits and rules as the A2A methods, the URL
// checked at set time (§10.3), and a config of a task that has ended stored and delivering nothing (D111). Delivery
// stays the lifecycle's (§10.2): /mcp delivers nothing itself.
type PushConfigs interface {
	Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error)
	List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error)
	Delete(ctx context.Context, taskID a2a.TaskID, configID string) error
}

// PushEntry is one push config as the push tools show it: its id, its URL, whether it has a token and its
// authentication scheme; never the token or the credentials, which are the client's own secrets.
type PushEntry struct {
	ConfigID string `json:"configId"`
	URL      string `json:"url"`
	Token    bool   `json:"token"`
	Scheme   string `json:"scheme,omitempty"`
}

// PushResult is the push tools' structuredContent: the task's configs (push_set: the one it set).
type PushResult struct {
	TaskID  string      `json:"taskId"`
	Configs []PushEntry `json:"configs"`
}

func pushEntry(c *a2a.PushConfig) PushEntry {
	e := PushEntry{ConfigID: c.ID, URL: c.URL, Token: c.Token != ""}
	if c.Auth != nil {
		e.Scheme = c.Auth.Scheme
	}
	return e
}

// pushTool answers push_set, push_list and push_delete (§12.2, D337) for a hire task of the principal (the tools'
// scoping; anything else is UnknownTask): the A2A push config methods' store with the principal on the context, as the
// A2A route calls it; an A2A refusal (a URL §10.3 refuses, a limit of §10.1, an invalid header value) is isError with
// its message, and nothing is stored.
func (h *Handler) pushTool(rq *request, name string, args json.RawMessage) {
	if h.deps.Push == nil {
		rq.result(errorResult("push notifications are not available on this gate", nil))
		return
	}
	var (
		set      PushSetArgs
		configID string
		err      error
	)
	switch name {
	case ToolPushSet:
		set, err = parsePushSetArgs(args)
	case ToolPushList:
		set.TaskID, err = parseTaskOnly(args)
	case ToolPushDelete:
		set.TaskID, configID, err = parsePushDeleteArgs(args)
	}
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.loadTool(rq, set.TaskID)
	switch {
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	case row == nil:
		rq.result(errorResult(UnknownTask, nil))
		return
	}
	ctx := auth.WithPrincipal(rq.ctx(), rq.p)
	id := a2a.TaskID(row.ID.String())
	out := PushResult{TaskID: row.ID.String(), Configs: []PushEntry{}}
	switch name {
	case ToolPushSet:
		cfg := set.Config
		cfg.TaskID = id
		saved, err := h.deps.Push.Save(ctx, id, &cfg)
		if err != nil {
			h.pushRefused(rq, err)
			return
		}
		out.Configs = append(out.Configs, pushEntry(saved))
	case ToolPushDelete:
		if err := h.deps.Push.Delete(ctx, id, configID); err != nil {
			h.pushRefused(rq, err)
			return
		}
		fallthrough
	case ToolPushList:
		list, err := h.deps.Push.List(ctx, id)
		if err != nil {
			h.pushRefused(rq, err)
			return
		}
		for _, c := range list {
			out.Configs = append(out.Configs, pushEntry(c))
		}
	}
	b, _ := json.Marshal(out)
	rq.result(textResult(string(b), out, nil))
}

// pushRefused answers an error of the push config store: an A2A refusal as isError with its message (a task deleted
// meanwhile as UnknownTask), anything else as the gate's own failure.
func (h *Handler) pushRefused(rq *request, err error) {
	var ae *a2a.Error
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		rq.result(errorResult(UnknownTask, nil))
	case errors.As(err, &ae) || errors.Is(err, a2a.ErrInvalidParams) || errors.Is(err, a2a.ErrUnsupportedOperation):
		msg := err.Error()
		if ae != nil && ae.Message != "" {
			msg = ae.Message
		}
		rq.result(errorResult(msg+"; nothing was stored.", nil))
	default:
		h.internal(rq, "serve a push config", err)
	}
}

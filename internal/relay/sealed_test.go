package relay

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
)

// sealedEnv is the relay for a service whose sealed mode is mode (services.sealed and the ServiceRef).
func sealedEnv(t *testing.T, mode string) *env {
	t.Helper()
	e := newEnv(t, envOptions{})
	e.exec(`update kontra.services set sealed = $1 where tenant_id = $2 and name = $3`, mode, e.svc.TenantID, e.svc.Name)
	e.svc.Sealed = mode
	e.intc.Svc = e.svc
	return e
}

// envelopePart is a part in the sealed envelope's raw form (ext-sealed.md §3).
func envelopePart() *a2a.Part {
	return &a2a.Part{Content: a2a.Raw("age-encryption.org/v1 not really"), MediaType: hooks.SealedMediaType,
		Metadata: map[string]any{hooks.KeySealed: map[string]any{"original": "text/plain"}}}
}

// sealed_unsealed_part (ext-sealed.md §5 "Gate", §9): a required service refuses a message with an unsealed part with
// InvalidRequest before any task row; a part claiming the envelope type in another form than raw is InvalidRequest on
// any service; a sealed raw part is accepted.
func TestSealedUnsealedPart(t *testing.T) {
	e := sealedEnv(t, sealedRequired)
	agent, b, info := newAgent(t, echoAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	h := e.handler()
	plain := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: plain}); !errors.Is(err, a2a.ErrInvalidRequest) {
		t.Fatalf("an unsealed part to a required service: %v", err)
	}
	mixed := a2a.NewMessage(a2a.MessageRoleUser, envelopePart(), a2a.NewTextPart("plain beside it"))
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: mixed}); !errors.Is(err, a2a.ErrInvalidRequest) {
		t.Fatalf("an unsealed part beside a sealed one: %v", err)
	}
	for _, form := range []*a2a.Part{
		{Content: a2a.Text("ciphertext as text"), MediaType: hooks.SealedMediaType},
		{Content: a2a.URL("https://example.com/c"), MediaType: "Application/VND.kontra.sealed+age; v=\"1\""},
		{Content: a2a.Data{Value: map[string]any{"x": 1}}, MediaType: hooks.SealedMediaType},
	} {
		msg := a2a.NewMessage(a2a.MessageRoleUser, form)
		if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg}); !errors.Is(err, a2a.ErrInvalidRequest) {
			t.Errorf("a sealed type in a form other than raw (%T): %v", form.Content, err)
		}
	}
	if n := e.countRows("tasks"); n != 0 {
		t.Fatalf("%d task rows for refused messages", n)
	}
	sealedMsg := a2a.NewMessage(a2a.MessageRoleUser, envelopePart())
	sealedMsg.Metadata = map[string]any{hooks.KeySealedReplyTo: "age1replyto"}
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: sealedMsg})
	if err != nil {
		t.Fatalf("a sealed message: %v", err)
	}
	if _, ok := res.(*a2a.Task); !ok {
		t.Fatalf("%T", res)
	}
	e.queue.wait(t)
	sent := agent.sentMessages(t)
	if len(sent) != 1 || sent[0].Parts[0].MediaType != hooks.SealedMediaType || sent[0].Metadata[hooks.KeySealedReplyTo] != "age1replyto" {
		t.Fatalf("upstream got %+v", sent)
	}
	if _, ok := sent[0].Parts[0].Metadata[hooks.KeySealed]; !ok {
		t.Error("the envelope's kontra.sealed metadata was stripped")
	}
}

// sealed: {required: false}: an unsealed part passes with metadata["kontra.sealed"] = false, whatever the caller set.
func TestSealedOptionalMarksUnsealedParts(t *testing.T) {
	e := sealedEnv(t, sealedOptional)
	agent, b, info := newAgent(t, echoAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"), envelopePart())
	msg.Parts[0].Metadata = map[string]any{hooks.KeySealed: map[string]any{"original": "spoof"}}
	if _, err := e.handler().SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg}); err != nil {
		t.Fatal(err)
	}
	e.queue.wait(t)
	sent := agent.sentMessages(t)[0]
	if v, ok := sent.Parts[0].Metadata[hooks.KeySealed]; !ok || v != false {
		t.Errorf("the unsealed part's kontra.sealed: %v", sent.Parts[0].Metadata)
	}
	if v := sent.Parts[1].Metadata[hooks.KeySealed]; v == false || v == nil {
		t.Errorf("the sealed part's kontra.sealed: %v", sent.Parts[1].Metadata)
	}
}

// A service that answers with the envelope type in a form other than raw fails the task upstream_error (§3).
func TestSealedFormFromService(t *testing.T) {
	e := newEnv(t, envOptions{})
	bad := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec,
				&a2a.Part{Content: a2a.Text("not raw"), MediaType: hooks.SealedMediaType})), nil)
		}
	})
	_, b, info := newAgent(t, bad, agentConfig{})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.dispatch(context.Background(), id, 1, wid, false)
	row := e.row(id)
	if row.State != store.StateFailed || !strings.Contains(e.logText(id), `"kontra.failure":"upstream_error"`) {
		t.Fatalf("state %s: %s", row.State, e.logText(id))
	}
}

// A hosting node's refusal (ext-sealed.md §6: 400, Kontra-Node: sealed_rejected) fails the task sealed_rejected, before
// and after the first upstream task id, never retried: the attempt's outcome is sealed_rejected, and an audit row
// names the task and the node's reason.
func TestTunnelSealedRejected(t *testing.T) {
	for _, after := range []bool{false, true} {
		s1 := newSession()
		e, _, nodes := tunnelEnv(t, envOptions{}, false, askingAgent(), s1)
		n := nodes[s1]
		n.setAnswer(func(method string, _ json.RawMessage, k int, w http.ResponseWriter) bool {
			if after && method != "GetTask" {
				return false // the send goes through; the first poll is refused
			}
			w.Header().Set(tunnel.HeaderSealedReject, "wrong_job")
			nodeAnswer(w, http.StatusBadRequest, tunnel.NodeSealedRejected)
			return true
		})
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
		res := e.dispatch(context.Background(), id, 1, wid, false)
		if res.Outcome == lifecycle.DispatchRetryable {
			t.Fatalf("after=%v: retried %+v", after, res)
		}
		row := e.row(id)
		if after && row.State != store.StateFailed {
			// The polling dispatch hands the turn back at input-required; poll until the refusal.
			res = e.dispatch(context.Background(), id, 1, wid, true)
			row = e.row(id)
		}
		if row.State != store.StateFailed || !strings.Contains(e.logText(id), `"kontra.failure":"sealed_rejected"`) {
			t.Fatalf("after=%v: %s %s (%+v)", after, row.State, e.logText(id), res)
		}
		if a := e.attempt(id, 1); a.Outcome != store.OutcomeSealedRejected {
			t.Fatalf("after=%v: attempt %+v", after, a)
		}
		rows, err := audit.Verify(context.Background(), e.pool)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			s := string(r.RowJCS)
			if strings.Contains(s, `"event":"sealed.rejected"`) && strings.Contains(s, string(id)) && strings.Contains(s, `"reason":"wrong_job"`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("after=%v: no audit row for the rejection", after)
		}
	}
}

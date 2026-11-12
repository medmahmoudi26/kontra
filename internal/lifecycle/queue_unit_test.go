package lifecycle

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// D362: a new message the caller awaits the answer to starts provisional; returnImmediately and a /mcp hire are
// exposed at admission.
func TestProvisional(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	for _, c := range []struct {
		name  string
		req   *a2a.SendMessageRequest
		hired bool
		want  bool
	}{
		{"blocking", &a2a.SendMessageRequest{Message: msg}, false, true},
		{"config without returnImmediately", &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{}}, false, true},
		{"returnImmediately", &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}, false, false},
		{"hire", &a2a.SendMessageRequest{Message: msg}, true, false},
	} {
		if got := provisional(c.req, c.hired); got != c.want {
			t.Errorf("%s: provisional %v, want %v", c.name, got, c.want)
		}
	}
}

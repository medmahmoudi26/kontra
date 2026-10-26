package obs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestJSONLoggerAndRedaction(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, FormatJSON, slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("hidden")
	l.Info("dispatch", append(Task("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11", 2),
		slog.String("Authorization", "Bearer kk_acme_secret"),
		slog.Group("request", slog.String("Cookie", "s=1"), slog.String("path", "/agents/x")),
		slog.String("postgres", "postgres://kontra:pw@db/kontra"),
		slog.String("set-cookie", "x"),
		slog.String(KeyService, "hello"))...)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (debug is below the level):\n%s", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, lines[0])
	}
	if rec["msg"] != "dispatch" || rec["level"] != "INFO" || rec["time"] == nil {
		t.Errorf("record = %v", rec)
	}
	if rec[KeyTaskID] != "0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11" || rec[KeyAttempt] != 2.0 || rec[KeyService] != "hello" {
		t.Errorf("task attributes = %v", rec)
	}
	for _, k := range []string{"Authorization", "postgres", "set-cookie"} {
		if rec[k] != Redacted {
			t.Errorf("%s = %v, want %s", k, rec[k], Redacted)
		}
	}
	if req := rec["request"].(map[string]any); req["Cookie"] != Redacted || req["path"] != "/agents/x" {
		t.Errorf("request group = %v", req)
	}
	if strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), ":pw@") {
		t.Errorf("a credential reached the log: %s", buf.String())
	}
}

// The backstop also covers a sensitive group (values at any depth below it), keys that only contain a
// credential word, a URL's password under any key, and maps logged with slog.Any.
func TestRedactionBackstop(t *testing.T) {
	for _, f := range []Format{FormatJSON, FormatText} {
		var buf bytes.Buffer
		l, err := NewLogger(&buf, f, slog.LevelInfo)
		if err != nil {
			t.Fatal(err)
		}
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer secret8")
		hdr.Set("X-Api-Key", "secret9")
		hdr.Set("Accept", "text/event-stream")
		l.Info("x",
			slog.Group("headers", slog.String("X-Api-Key", "kk_secret1"), slog.String("Authorization", "Bearer secret2"),
				slog.Group("inner", slog.String("note", "secret10"))),
			slog.String("access_token", "secret3"),
			slog.String("url", "postgres://u:secret4@h/db"),
			slog.String("x-api-key", "secret5"),
			slog.String("client_secret", "secret6"),
			slog.Any("cfg", map[string]any{"postgres": "postgres://u:secret7@h/db", "nested": map[string]string{"password": "secret11"}, "ok": "visible1"}),
			slog.Any("req", hdr),
			slog.String("dsn_kv", "host=db user=k password=secret12 dbname=k"),
			slog.Any("error", errors.New("dial https://bob:secret13@seller.example/a2a: refused")),
			slog.String(KeyTaskID, "visible2"),
			slog.String("path", "/agents/x"),
		)
		l.WithGroup("cookie").Info("y", slog.String("session", "secret14"))
		l.With(slog.String("refresh_token", "secret15")).Info("z")
		out := buf.String()
		for i := 1; i <= 15; i++ {
			if strings.Contains(out, fmt.Sprintf("secret%d", i)) {
				t.Errorf("%s: secret%d reached the log:\n%s", f, i, out)
			}
		}
		for _, want := range []string{"visible1", "visible2", "/agents/x", "text/event-stream", "seller.example/a2a", "postgres://u:"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q was redacted too:\n%s", f, want, out)
			}
		}
	}
}

func TestFormats(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, FormatText, slog.LevelDebug)
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("hello", "token", "t")
	if !strings.Contains(buf.String(), "msg=hello") || !strings.Contains(buf.String(), "token="+Redacted) {
		t.Errorf("text line = %q", buf.String())
	}
	if _, err := NewLogger(&buf, "", slog.LevelInfo); err != nil {
		t.Errorf("the default format: %v", err)
	}
	if _, err := NewLogger(&buf, "xml", slog.LevelInfo); err == nil {
		t.Error("an unknown format was accepted")
	}
}

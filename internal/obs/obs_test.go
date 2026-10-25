package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
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

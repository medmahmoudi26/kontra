package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/unimpl"
)

func TestSubcommands(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		args        []string
		code        int
		out, errOut string
	}{
		{[]string{"version"}, 0, "kontra", ""},
		{nil, 2, "", "usage:"},
		{[]string{"nope"}, 2, "", "usage:"},
		{[]string{"seal"}, 1, "", "kontra seal: not until M3"},
		{[]string{"console"}, 1, "", "kontra console: not until M4"},
		{[]string{"login"}, 1, "", "kontra login: not until M4"},
		{[]string{"publish"}, 1, "", "kontra publish: not until M5"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(ctx, c.args, &stdout, &stderr); code != c.code ||
			!strings.Contains(stdout.String(), c.out) || !strings.Contains(stderr.String(), c.errOut) {
			t.Errorf("kontra %q = %d, stdout %q, stderr %q; want %d, %q, %q", c.args, code, stdout.String(), stderr.String(),
				c.code, c.out, c.errOut)
		}
	}
}

// TestConnectIsWired checks that connect reaches internal/node (WP-06 replaces this with the flag tests).
func TestConnectIsWired(t *testing.T) {
	_, err := node.ParseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999"}, func(string) string { return "" })
	if !errors.Is(err, unimpl.ErrNotImplemented) {
		t.Skip("node.ParseArgs is built: the skeleton check no longer applies")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"connect", "--serve", "a2a:hello=http://127.0.0.1:9999"}, &bytes.Buffer{}, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "WP-06") {
		t.Errorf("connect = %d %q; want 1 and the WP-06 marker", code, stderr.String())
	}
}

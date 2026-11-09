package gatemcp

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// describe's capabilities (D339): the served card's capabilities, modes, skills (with their examples, the suggested
// first messages) and extensions, normalized. A card that still carries the withdrawn input-schema extension lists it
// as an extension and nothing more: no inputSchema (D350). A card that is none has none.
func TestCardCapabilities(t *testing.T) {
	card := json.RawMessage(`{"name":"booking","capabilities":{"streaming":true,"pushNotifications":false,
		"extensions":[{"uri":"https://kontra.sh/ext/input-schema/v1","required":false,"params":{"schema":{"type":"object",
		"properties":{"topic":{"type":"string"}},"required":["topic"]}}},{"uri":"https://seller.example/ext/x","required":true}]},
		"defaultInputModes":["text/plain","application/json"],"defaultOutputModes":["text/plain"],
		"skills":[{"id":"book","name":"Book","description":"Books a table","tags":["food"],"examples":["a table for two"],
		"inputModes":["application/json"],"extra":1}]}`)
	c := cardCapabilities(card)
	if c == nil || c.Streaming == nil || !*c.Streaming || c.PushNotifications == nil || *c.PushNotifications ||
		!reflect.DeepEqual(c.InputModes, []string{"text/plain", "application/json"}) || !reflect.DeepEqual(c.OutputModes, []string{"text/plain"}) {
		t.Fatalf("capabilities %+v", c)
	}
	if !reflect.DeepEqual(c.Skills, []SkillSummary{{ID: "book", Name: "Book", Description: "Books a table", Tags: []string{"food"},
		Examples: []string{"a table for two"}, InputModes: []string{"application/json"}}}) {
		t.Errorf("skills %+v", c.Skills)
	}
	if !reflect.DeepEqual(c.Extensions, []ExtensionInCard{{URI: "https://kontra.sh/ext/input-schema/v1"}, {URI: "https://seller.example/ext/x", Required: true}}) {
		t.Errorf("extensions %+v", c.Extensions)
	}
	if raw, err := json.Marshal(c); err != nil || bytes.Contains(raw, []byte("inputSchema")) || bytes.Contains(raw, []byte(`"topic"`)) {
		t.Errorf("capabilities carry the input schema: %s", raw)
	}
	if cardCapabilities(nil) != nil || cardCapabilities(json.RawMessage(`[1]`)) != nil {
		t.Error("a card that is none has capabilities")
	}
}

// The tools' arguments are read with exact keys against their schemas (D332..D337).
func TestToolArguments(t *testing.T) {
	const id = "019a0000-0000-7000-8000-000000000001"
	good := map[string]func(json.RawMessage) error{
		`{"taskId":"` + id + `","after":0,"wait":25}`: func(b json.RawMessage) error { _, err := parseStatusArgs(b); return err },
		`{"state":"ended","service":"t/hello","contextId":"c","cursor":"x"}`: func(b json.RawMessage) error {
			_, err := parseTasksArgs(b)
			return err
		},
		`{"state":"TASK_STATE_INPUT_REQUIRED"}`:     func(b json.RawMessage) error { _, err := parseTasksArgs(b); return err },
		`{"taskId":"` + id + `","artifactId":"a1"}`: func(b json.RawMessage) error { _, err := parseArtifactArgs(b); return err },
		`{"taskId":"` + id + `","url":"https://h.example","authentication":{"scheme":"Bearer"}}`: func(b json.RawMessage) error {
			_, err := parsePushSetArgs(b)
			return err
		},
		`{"taskId":"` + id + `","configId":"c1"}`: func(b json.RawMessage) error { _, _, err := parsePushDeleteArgs(b); return err },
	}
	for args, parse := range good {
		if err := parse(json.RawMessage(args)); err != nil {
			t.Errorf("%s: %v", args, err)
		}
	}
	bad := map[string]func(json.RawMessage) error{
		`{"taskId":"` + id + `","wait":2.5}`:            func(b json.RawMessage) error { _, err := parseStatusArgs(b); return err },
		`{"taskId":"` + id + `","after":1e3}`:           func(b json.RawMessage) error { _, err := parseStatusArgs(b); return err },
		`{"taskId":"` + id + `","taskId":"` + id + `"}`: func(b json.RawMessage) error { _, err := parseStatusArgs(b); return err },
		`{"state":"done"}`:                              func(b json.RawMessage) error { _, err := parseTasksArgs(b); return err },
		`{"taskId":"` + id + `"}`:                       func(b json.RawMessage) error { _, err := parseArtifactArgs(b); return err },
		`{"taskId":"` + id + `","url":"https://h.example","authentication":{}}`: func(b json.RawMessage) error {
			_, err := parsePushSetArgs(b)
			return err
		},
		`{"taskId":"` + id + `","url":""}`:      func(b json.RawMessage) error { _, err := parsePushSetArgs(b); return err },
		`{"taskId":"` + id + `","configId":""}`: func(b json.RawMessage) error { _, _, err := parsePushDeleteArgs(b); return err },
	}
	for args, parse := range bad {
		if err := parse(json.RawMessage(args)); err == nil {
			t.Errorf("%s accepted", args)
		}
	}
	if got := statesOf("TASK_STATE_INPUT_REQUIRED"); len(got) != 2 {
		t.Errorf("input-required selects %v, want quoted and input-required", got)
	}
}

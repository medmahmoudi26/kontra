package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/identity"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/wire"
)

// TODAY'S TRUTH, PINNED: the backing workflow's dispatch choices against
// shared/conformance/dispatch.json (ADR 0067). Phase 2 moves these into the callers; each caller
// drives the same rows, so the move cannot change what a dispatch does.

type dispatchCorpus struct {
	Activities map[string]string `json:"activities"`
	ActorID    struct {
		Cases []struct {
			Why            string `json:"why"`
			IdempotencyKey string `json:"idempotency_key"`
			SessionID      string `json:"session_id"`
			RunID          string `json:"run_id"`
			NodeID         string `json:"node_id"`
			Expect         string `json:"expect"`
		} `json:"cases"`
		Refused []struct {
			Why string `json:"why"`
		} `json:"refused"`
	} `json:"actor_id"`
	RunOptions struct {
		StartToCloseSeconds          int `json:"start_to_close_seconds"`
		ScopedScheduleToStartSeconds int `json:"scoped_schedule_to_start_seconds"`
		Retry                        struct {
			MaximumAttempts        int32 `json:"maximum_attempts"`
			InitialIntervalSeconds int   `json:"initial_interval_seconds"`
		} `json:"retry"`
		Heartbeat []struct {
			Why       string `json:"why"`
			Requested int32  `json:"requested_seconds"`
			Expect    int    `json:"expect_seconds"`
		} `json:"heartbeat"`
	} `json:"run_options"`
	CloseOptions struct {
		StartToCloseSeconds int `json:"start_to_close_seconds"`
		Retry               struct {
			MaximumAttempts int32 `json:"maximum_attempts"`
		} `json:"retry"`
	} `json:"close_options"`
	BlobOptions struct {
		StartToCloseSeconds int `json:"start_to_close_seconds"`
	} `json:"blob_options"`
	Queue struct {
		Cases []struct {
			Why       string `json:"why"`
			Name      string `json:"name"`
			Version   string `json:"version"`
			SessionID string `json:"session_id"`
			Expect    string `json:"expect"`
		} `json:"cases"`
	} `json:"queue"`
}

func loadDispatchCorpus(t *testing.T) dispatchCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../shared/conformance/dispatch.json")
	if err != nil {
		t.Fatal(err)
	}
	var c dispatchCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheDispatchMatchesTheCorpus(t *testing.T) {
	c := loadDispatchCorpus(t)
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }

	if c.Activities["fetch_blob"] != identity.FetchBlobActivity || c.Activities["store_blob"] != identity.StoreBlobActivity {
		t.Errorf("blob activity names drifted: corpus %v", c.Activities)
	}

	for _, k := range c.ActorID.Cases {
		t.Run("actor_id/"+k.Why, func(t *testing.T) {
			got, err := deriveActorID(wire.EntryInput{IdempotencyKey: k.IdempotencyKey, SessionID: k.SessionID, RunID: k.RunID, NodeID: k.NodeID})
			if err != nil || got != k.Expect {
				t.Fatalf("got %q, %v; want %q", got, err, k.Expect)
			}
		})
	}
	for _, k := range c.ActorID.Refused {
		t.Run("actor_id_refused/"+k.Why, func(t *testing.T) {
			if _, err := deriveActorID(wire.EntryInput{}); err == nil {
				t.Fatal("an input with nothing to key by must be refused")
			}
		})
	}

	for _, h := range c.RunOptions.Heartbeat {
		t.Run("heartbeat/"+h.Why, func(t *testing.T) {
			if got := heartbeatFor(h.Requested); got != sec(h.Expect) {
				t.Fatalf("heartbeatFor(%d) = %s, want %ds", h.Requested, got, h.Expect)
			}
		})
	}

	for _, q := range c.Queue.Cases {
		t.Run("queue/"+q.Why, func(t *testing.T) {
			opts := runActivityOptions(identity.SharedQueue(q.Name, q.Version), q.SessionID, 0)
			if opts.TaskQueue != q.Expect {
				t.Fatalf("RunBatch queue %q, want %q", opts.TaskQueue, q.Expect)
			}
			if opts.StartToCloseTimeout != sec(c.RunOptions.StartToCloseSeconds) {
				t.Errorf("StartToClose %s", opts.StartToCloseTimeout)
			}
			if opts.RetryPolicy == nil || opts.RetryPolicy.MaximumAttempts != c.RunOptions.Retry.MaximumAttempts ||
				opts.RetryPolicy.InitialInterval != sec(c.RunOptions.Retry.InitialIntervalSeconds) {
				t.Errorf("retry %+v", opts.RetryPolicy)
			}
			wantS2S := time.Duration(0)
			if q.SessionID != "" {
				wantS2S = sec(c.RunOptions.ScopedScheduleToStartSeconds)
			}
			if opts.ScheduleToStartTimeout != wantS2S {
				t.Errorf("ScheduleToStart %s, want %s", opts.ScheduleToStartTimeout, wantS2S)
			}
		})
	}

	closeOpts := closeActivityOptions("q")
	if closeOpts.TaskQueue != "q" || closeOpts.StartToCloseTimeout != sec(c.CloseOptions.StartToCloseSeconds) ||
		closeOpts.RetryPolicy == nil || closeOpts.RetryPolicy.MaximumAttempts != c.CloseOptions.Retry.MaximumAttempts {
		t.Errorf("close options %+v", closeOpts)
	}
	if blobActivityOptions().StartToCloseTimeout != sec(c.BlobOptions.StartToCloseSeconds) {
		t.Errorf("blob options %+v", blobActivityOptions())
	}
}

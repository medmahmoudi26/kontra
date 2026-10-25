// Package push stores push notification configs and delivers them (spec/relay.md §10.1–§10.2, D16, D55).
//
// Configs live in push_configs per gate task; the gate, not a2a-go's executor, validates a config's URL
// when it is set. Delivery is the lifecycle's, never the relaying replica's: one push_webhook activity per
// live config on kontra-push tails the log from its last delivered seq and POSTs the task snapshot at each
// seq, in order, at least once. a2a-go's inline push is disabled with NoopSender.
package push

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/ssrf"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Limits on configs (spec §10.1); beyond them, InvalidParams.
const (
	MaxPerTask  = 10
	MaxPerBuyer = 256 // delivering configs
)

// Signaler announces a config change to the task's workflow (kontra.push); lifecycle.Client implements it.
type Signaler interface {
	SignalPush(ctx context.Context, taskID a2a.TaskID, s lifecycle.PushSignal) error
}

// ConfigStore implements a2a-go's push.ConfigStore over push_configs. Save is idempotent: a config without
// an id is keyed by (task, URL, token hash), so a2a-go's re-save on every turn neither duplicates a row nor
// starts a second delivery. After a Save that inserted a row, or a Delete, commits, it sends kontra.push.
// config holds the caller's webhook credentials: it is never logged and never in an operator read.
type ConfigStore struct {
	db     *pgxpool.Pool
	signal Signaler
	policy ssrf.Policy
}

var (
	_ a2apush.ConfigStore        = (*ConfigStore)(nil)
	_ lifecycle.PushConfigLister = (*ConfigStore)(nil)
)

// NewConfigStore returns the store; policy is push.allow_private.
func NewConfigStore(db *pgxpool.Pool, signal Signaler, policy ssrf.Policy) *ConfigStore {
	return &ConfigStore{db: db, signal: signal, policy: policy}
}

// Save implements push.ConfigStore.
func (s *ConfigStore) Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, unimpl.Error("push.ConfigStore.Save", "WP-07")
}

// Get implements push.ConfigStore.
func (s *ConfigStore) Get(ctx context.Context, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error) {
	return nil, unimpl.Error("push.ConfigStore.Get", "WP-07")
}

// List implements push.ConfigStore.
func (s *ConfigStore) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	return nil, unimpl.Error("push.ConfigStore.List", "WP-07")
}

// Delete implements push.ConfigStore; deleting a missing config is not an error (spec §3.1.10).
func (s *ConfigStore) Delete(ctx context.Context, taskID a2a.TaskID, configID string) error {
	return unimpl.Error("push.ConfigStore.Delete", "WP-07")
}

// DeleteAll implements push.ConfigStore.
func (s *ConfigStore) DeleteAll(ctx context.Context, taskID a2a.TaskID) error {
	return unimpl.Error("push.ConfigStore.DeleteAll", "WP-07")
}

// LiveConfigIDs implements lifecycle.PushConfigLister.
func (s *ConfigStore) LiveConfigIDs(ctx context.Context, taskID uuid.UUID) ([]string, error) {
	return nil, unimpl.Error("push.ConfigStore.LiveConfigIDs", "WP-07")
}

// CheckURL is the set-time check of a config URL: https and the SSRF rules (spec §10.3). A redirecting
// URL is accepted; its redirect is never followed (D60).
func CheckURL(ctx context.Context, rawURL string, policy ssrf.Policy) error {
	return unimpl.Error("push.CheckURL", "WP-07")
}

// NoopSender is the push.Sender a2a-go's handler is built with, so push CRUD works while delivery stays
// with the lifecycle (spec §15).
type NoopSender struct{}

var _ a2apush.Sender = NoopSender{}

// SendPush implements push.Sender: it does nothing.
func (NoopSender) SendPush(ctx context.Context, config *a2a.PushConfig, event a2a.Event) error {
	return nil
}

// Deliverer implements lifecycle.PushActivities.
type Deliverer struct {
	log    *store.Log
	db     *pgxpool.Pool
	policy ssrf.Policy
}

var _ lifecycle.PushActivities = (*Deliverer)(nil)

// NewDeliverer returns the push_webhook implementation.
func NewDeliverer(log *store.Log, db *pgxpool.Pool, policy ssrf.Policy) *Deliverer {
	return &Deliverer{log: log, db: db, policy: policy}
}

// PushWebhook delivers, for each event after the larger of push_configs.delivered_seq and the heartbeat
// details, the task snapshot at that seq as {"task": …}: one POST, 10 s timeout, Content-Type
// application/a2a+json, the config's authentication as Authorization and its token as both
// A2A-Notification-Token and X-A2A-Notification-Token. Any 2xx is delivered; a 3xx is a failed delivery
// whose target receives no connection; anything else is retried 1 s doubling to 5 min for at most 24 h
// from the first failure of that seq, then the config is marked failed. Runs of artifact events may be
// coalesced into the snapshot at the last seq of the run; every status change and the terminal event are
// delivered. Deliveries to one destination host are capped at 10 a second across the gate. It ends after
// the terminal snapshot or when the config is deleted.
func (d *Deliverer) PushWebhook(ctx context.Context, in lifecycle.PushInput) (lifecycle.PushResult, error) {
	return lifecycle.PushResult{}, unimpl.Error("push.Deliverer.PushWebhook", "WP-07")
}

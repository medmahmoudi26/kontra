// workers.go — `kontra workers list`: the catalog joined with LIVE Temporal pollers.
//
// Registration says an actor EXISTS; only a poller on its queue says it can RUN. This
// command surfaces that gap (the classic "registered but no worker" trap) per queue,
// plus the orchestrator's own interpreter queue.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/internal/queues"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/sdk/go/temporaltls"
)

const orchestratorQueue = "kontra-orchestrator"

type pollerInfo struct {
	Identity   string
	LastAccess time.Time
}

// queueDescriber is the Temporal seam — an interface so tests never dial anything.
type queueDescriber interface {
	Pollers(ctx context.Context, queue string, qtype enumspb.TaskQueueType) ([]pollerInfo, error)
	Close()
}

// newDescriber dials Temporal ($KONTRA_ADDRESS / $KONTRA_NAMESPACE); a func var so
// tests (and pollerCount fakes) can swap it out.
var newDescriber = func() (queueDescriber, error) { return newDescriberIn(config.TemporalNamespace()) }

// newDescriberIn asks one namespace for pollers (ADR 0051): a worker in any other namespace cannot
// take this namespace's work, so "a poller exists somewhere" is not the answer a preflight wants.
func newDescriberIn(namespace string) (queueDescriber, error) {
	conn, err := temporaltls.ConnectionOptions(nil)
	if err != nil {
		return nil, err
	}
	c, err := client.Dial(client.Options{HostPort: config.TemporalAddress(), Namespace: namespace, ConnectionOptions: conn})
	if err != nil {
		return nil, err
	}
	return &temporalDescriber{c: c, ns: namespace}, nil
}

type temporalDescriber struct {
	c  client.Client
	ns string
}

func (t *temporalDescriber) Pollers(ctx context.Context, queue string, qtype enumspb.TaskQueueType) ([]pollerInfo, error) {
	resp, err := t.c.WorkflowService().DescribeTaskQueue(ctx, &workflowservice.DescribeTaskQueueRequest{
		Namespace:     t.ns,
		TaskQueue:     &taskqueuepb.TaskQueue{Name: queue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
		TaskQueueType: qtype,
	})
	if err != nil {
		return nil, err
	}
	out := make([]pollerInfo, 0, len(resp.GetPollers()))
	for _, p := range resp.GetPollers() {
		out = append(out, pollerInfo{Identity: p.GetIdentity(), LastAccess: p.GetLastAccessTime().AsTime()})
	}
	return out, nil
}

func (t *temporalDescriber) Close() { t.c.Close() }

func cmdWorkers(args []string) error {
	if len(args) != 1 || args[0] != "list" {
		return errors.New("usage: kontra workers list")
	}
	// Fetch the catalog FIRST — if the orchestrator is down, surface that (and don't dial
	// Temporal needlessly) before reaching for pollers.
	var actors []actorRecord
	if err := newAPI(orchestratorURL()).getJSON("/api/actors", &actors); err != nil {
		return fmt.Errorf("GET /api/actors failed: %w", err)
	}
	d, err := newDescriber()
	if err != nil {
		// A dial failure is NOT "no workers" — fail loudly instead of printing (none).
		return fmt.Errorf("cannot reach temporal at %s: %w", config.TemporalAddress(), err)
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows := collectWorkers(ctx, actors, d)
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ACTOR\tVERSION\tQUEUE\tWORKERS\tLAST-POLL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Actor, r.Version, r.Queue, r.workersText(), r.lastText())
	}
	return w.Flush()
}

// workerRow is one queue's live-poller state — the catalog row joined with Temporal's
// pollers. `kontra workers list` renders it as a table; the `list_workers` MCP tool
// returns it as JSON. `Live` is the count of distinct poller identities (0 = registered
// but no worker); `Error` is set when the Temporal lookup itself failed for this queue.
type workerRow struct {
	Actor    string    `json:"actor"`
	Version  string    `json:"version"`
	Queue    string    `json:"queue"`
	Live     int       `json:"live"`
	Workers  []string  `json:"workers"`           // distinct poller identities
	LastPoll time.Time `json:"lastPoll,omitzero"` // freshest poll across identities
	Error    string    `json:"error,omitempty"`   // Temporal describe error, if any
}

// collectWorkers joins the catalog (already fetched by the caller — so a dead orchestrator
// surfaces before Temporal is dialed) with LIVE Temporal pollers, one row per actor queue
// plus the orchestrator's own queue. The single source of truth for both the CLI table and
// the MCP tool — registration says an actor EXISTS; a poller says it can RUN.
func collectWorkers(ctx context.Context, actors []actorRecord, d queueDescriber) []workerRow {
	sort.Slice(actors, func(i, j int) bool { return actors[i].Key < actors[j].Key })
	rows := make([]workerRow, 0, len(actors)+1)
	for _, a := range actors {
		rows = append(rows, describeQueue(ctx, d, a.Name, a.Version, queues.Shared(a.Name, a.Version)))
	}
	rows = append(rows, describeQueue(ctx, d, "(orchestrator)", "-", orchestratorQueue))
	return rows
}

// describeQueue folds one queue's WORKFLOW + ACTIVITY pollers (a live actor worker polls
// both; NEXUS is skipped — the other two already prove liveness) into a workerRow: the
// distinct poller identities and the freshest poll time.
func describeQueue(ctx context.Context, d queueDescriber, name, version, queue string) workerRow {
	seen := map[string]time.Time{}
	var describeErr error
	for _, qt := range []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY} {
		ps, err := d.Pollers(ctx, queue, qt)
		if err != nil {
			describeErr = err
			break
		}
		for _, p := range ps {
			if t, ok := seen[p.Identity]; !ok || p.LastAccess.After(t) {
				seen[p.Identity] = p.LastAccess
			}
		}
	}
	row := workerRow{Actor: name, Version: version, Queue: queue}
	if describeErr != nil {
		row.Error = describeErr.Error()
		return row
	}
	ids := make([]string, 0, len(seen))
	var latest time.Time
	for id, t := range seen {
		ids = append(ids, id)
		if t.After(latest) {
			latest = t
		}
	}
	sort.Strings(ids)
	row.Workers = ids
	row.Live = len(ids)
	row.LastPoll = latest
	return row
}

// workersText / lastText render a row's poller state for the CLI table (the MCP tool uses
// the struct fields directly).
func (r workerRow) workersText() string {
	switch {
	case r.Error != "":
		return "temporal error: " + r.Error
	case r.Live == 0:
		return "(none — registered but no live worker)"
	default:
		return strings.Join(r.Workers, ", ")
	}
}

func (r workerRow) lastText() string {
	if r.LastPoll.IsZero() {
		return "-"
	}
	return time.Since(r.LastPoll).Round(time.Second).String() + " ago"
}

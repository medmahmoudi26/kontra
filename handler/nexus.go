package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/nexus-rpc/sdk-go/nexus"
	nexusv1 "go.temporal.io/api/nexus/v1"
	operatorservice "go.temporal.io/api/operatorservice/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporalnexus"

	kontrav1temporal "github.com/medmahmoudi26/kontra/handler/_gen/kontra/v1/kontrav1temporal"
	"github.com/medmahmoudi26/kontra/handler/internal/identity"
	"github.com/medmahmoudi26/kontra/handler/internal/wire"
)

// newNexusService hand-wires the kontra.actor:run op against the wire types (byte-exact
// snake_case), NOT the proto-typed generated op — protojson would drift to camelCase and
// silently mismatch the orchestrator's wire. The op is registered under the generated
// op-name const; the backing workflow is registered under the generated workflow name.
func (a *Activities) newNexusService(queue string) (*nexus.Service, error) {
	op := temporalnexus.NewWorkflowRunOperation(
		kontrav1temporal.RunWorkflowOperationName, // "run"
		a.RunWorkflow,
		func(_ context.Context, in wire.EntryInput, _ nexus.StartOperationOptions) (client.StartWorkflowOptions, error) {
			return client.StartWorkflowOptions{ID: backingWorkflowID(in), TaskQueue: queue}, nil
		},
	)
	svc := nexus.NewService(identity.NexusServiceName)
	if err := svc.Register(op); err != nil {
		return nil, err
	}
	return svc, nil
}

// backingWorkflowID derives a deterministic-when-keyed id so a retried start attaches to the
// same run: idempotency key, else run/node joined, else a fresh uuid. Prefixed by actor name.
// os.Getenv is fine here — this runs in the operation handler, not workflow code.
//
// The prefix is "actor-". Nothing PARSES it — the execution list filters on WorkflowType, not on
// this — so it is a label for the Temporal UI, and a label that names the wrong thing is worse
// than no label at all.
func backingWorkflowID(in wire.EntryInput) string {
	base := in.IdempotencyKey
	if base == "" {
		base = joinNonEmpty(in.RunID, in.NodeID)
	}
	if base == "" {
		base = uuid.NewString()
	}
	return "actor-" + os.Getenv("KONTRA_ACTOR_NAME") + "-" + base
}

// joinNonEmpty joins the non-empty parts with "-".
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "-")
}

// ensureNexusEndpoint idempotently creates the actor's Nexus endpoint targeting its shared
// worker queue. An AlreadyExists is fine — and is now the EXPECTED answer.
//
// ── THIS IS A FALLBACK NOW, NOT THE OWNER ─────────────────────────────────────────────────────
//
// Creating the endpoint here made a caller's dispatch route a side effect of somebody having
// started a process. Three costs, all of them paid on this cluster:
//
//   - A caller written against an actor nobody had served yet failed at dispatch, on an endpoint
//     name that did not exist — reported as a Nexus error rather than as "that actor is not
//     registered here".
//   - Nothing owned the endpoints. Thirty-one outlived every worker that made them: live routes to
//     task queues nobody polls, indistinguishable in the endpoint list from real actors.
//   - You could not declare that an actor exists here without also running it.
//
// `kontra actor register` owns it now (control/orchestrator/src/nexusRegistry.ts), and forgetting the
// registration removes it. This call stays because a worker can be started outside that control
// plane — `kontra serve --actor` on a fleet Machine, a container someone runs by hand — and a
// worker polling a queue no endpoint addresses is a silent failure of exactly the kind the
// registration change exists to remove. It is idempotent, so in the normal path it is a no-op that
// reports `AlreadyExists` and moves on.
func ensureNexusEndpoint(ctx context.Context, c client.Client, namespace, name, version string) (string, error) {
	endpoint := identity.EndpointName(name, version)
	_, err := c.OperatorService().CreateNexusEndpoint(ctx, &operatorservice.CreateNexusEndpointRequest{
		Spec: &nexusv1.EndpointSpec{
			Name: endpoint,
			Target: &nexusv1.EndpointTarget{
				Variant: &nexusv1.EndpointTarget_Worker_{
					Worker: &nexusv1.EndpointTarget_Worker{
						Namespace: namespace,
						TaskQueue: identity.SharedQueue(name, version),
					},
				},
			},
		},
	})
	if err != nil {
		var exists *serviceerror.AlreadyExists
		if !errors.As(err, &exists) {
			return endpoint, err
		}
	}
	return endpoint, nil
}

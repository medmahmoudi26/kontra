package main

// Waiting for a workspace's Temporal namespace to exist (ADR 0051).
//
// The ORCHESTRATOR provisions namespaces, because it is the one place that registers a namespace's
// search attributes too, and an unregistered attribute hard-fails the first stamped start or upsert
// in that namespace. It does so for every workspace folder at boot and on a short rescan. So a
// workspace created a moment ago may not have its namespace yet when `kontra workflow serve` runs,
// and a worker polling a namespace Temporal does not know dies on its first poll. This waits, and
// says what it is waiting for, rather than starting a worker that will die.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/sdk/go/temporaltls"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// namespaceWait is how long a serve waits for the orchestrator to provision a namespace.
var namespaceWait = 90 * time.Second

// namespaceExists is a seam: the live answer asks Temporal.
var namespaceExists = liveNamespaceExists

func liveNamespaceExists(ctx context.Context, namespace string) (bool, error) {
	conn, err := temporaltls.ConnectionOptions(nil)
	if err != nil {
		return false, err
	}
	nc, err := client.NewNamespaceClient(client.Options{HostPort: config.TemporalAddress(), ConnectionOptions: conn})
	if err != nil {
		return false, err
	}
	defer nc.Close()
	if _, err := nc.Describe(ctx, namespace); err != nil {
		var nf *serviceerror.NamespaceNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func waitForNamespace(namespace string) error {
	deadline := time.Now().Add(namespaceWait)
	said := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := namespaceExists(ctx, namespace)
		cancel()
		if err != nil {
			return fmt.Errorf("cannot ask Temporal whether namespace %q exists: %w", namespace, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %q does not exist after %s. The orchestrator creates one for each "+
				"workspace folder under KONTRA_WORKSPACES; is it running, and does it see this workspace?", namespace, namespaceWait)
		}
		if !said {
			fmt.Fprintf(cliio.Stderr, "kontra: waiting for the orchestrator to create Temporal namespace %q for this workspace…\n", namespace)
			said = true
		}
		time.Sleep(2 * time.Second)
	}
}

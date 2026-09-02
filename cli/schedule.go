// schedule.go — `kontra schedule`: READ and STEER the Temporal Schedules this installation has.
//
// Temporal already owns durable, exactly-this-often scheduling: catch-up after downtime, an
// overlap policy, pause/resume, manual trigger, and a per-action history. Re-implementing any of
// that in kontra would mean a second source of truth for "is it running?", so this command is
// deliberately a thin, opinionated front-end over ScheduleClient rather than a scheduler.
//
// IT NO LONGER CREATES ONE. `create` scheduled a saved GRAPH: each firing started
// `scheduledGraphRun`, which asked the orchestrator to dispatch it. That workflow was deleted
// with the interpreter (4a69b27 removed `workflows/scheduledRun.ts`), and its task queue
// `kontra-orchestrator` has no worker, so a schedule created here could only ever fire into
// nothing. Nothing replaced it: there is no v2 way to put a cadence on work from this CLI.
//
// WHAT IT STILL STEERS is every Schedule something else made — today the retention sweep the
// orchestrator registers itself (`src/retention.ts`, ADR 0029 §5). Those are real, they fire, and
// pausing or triggering one is exactly what this command is opened for.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"
	"time"

	"go.temporal.io/sdk/client"
)

// newScheduleClient is a func var so tests never dial anything.
var newScheduleClient = func() (client.Client, error) {
	return client.Dial(client.Options{HostPort: temporalAddress(), Namespace: temporalNamespace()})
}

func cmdSchedule(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra schedule list|describe|delete|pause|resume|trigger")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return scheduleList(rest)
	case "describe":
		return scheduleDescribe(rest)
	case "delete", "pause", "resume", "trigger":
		return scheduleAct(sub, rest)
	default:
		return fmt.Errorf("unknown schedule subcommand %q (want list|describe|delete|pause|resume|trigger)", sub)
	}
}

func scheduleList(args []string) error {
	fs := flag.NewFlagSet("schedule list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newScheduleClient()
	if err != nil {
		return fmt.Errorf("dial temporal at %s: %w", temporalAddress(), err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	iter, err := c.ScheduleClient().List(ctx, client.ScheduleListOptions{PageSize: 100})
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCHEDULE\tSTATE\tRECENT ACTIONS\tNEXT")
	n := 0
	for iter.HasNext() {
		e, err := iter.Next()
		if err != nil {
			return err
		}
		n++
		state := "running"
		if e.Paused {
			state = "PAUSED"
		}
		next := "-"
		if len(e.NextActionTimes) > 0 {
			next = e.NextActionTimes[0].Local().Format("15:04:05")
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", e.ID, state, len(e.RecentActions), next)
	}
	if n == 0 {
		fmt.Fprintln(w, "(none)")
	}
	return w.Flush()
}

func scheduleDescribe(args []string) error {
	fs := flag.NewFlagSet("schedule describe", flag.ContinueOnError)
	id := fs.String("id", "", "schedule id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	c, err := newScheduleClient()
	if err != nil {
		return fmt.Errorf("dial temporal at %s: %w", temporalAddress(), err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := c.ScheduleClient().GetHandle(ctx, *id).Describe(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "schedule %s\n", *id)
	fmt.Fprintf(stdout, "  paused    %v\n", d.Schedule.State.Paused)
	if d.Schedule.State.Note != "" {
		fmt.Fprintf(stdout, "  note      %s\n", d.Schedule.State.Note)
	}
	if len(d.Schedule.Spec.Intervals) > 0 {
		fmt.Fprintf(stdout, "  every     %s\n", d.Schedule.Spec.Intervals[0].Every)
	}
	for _, cr := range d.Schedule.Spec.CronExpressions {
		fmt.Fprintf(stdout, "  cron      %s\n", cr)
	}
	fmt.Fprintf(stdout, "  overlap   %s\n", d.Schedule.Policy.Overlap)
	if a, ok := d.Schedule.Action.(*client.ScheduleWorkflowAction); ok {
		fmt.Fprintf(stdout, "  starts    %s on %s\n", a.Workflow, a.TaskQueue)
		for _, arg := range a.Args {
			fmt.Fprintf(stdout, "  args      %v\n", arg)
		}
	}
	fmt.Fprintf(stdout, "  running   %d\n", len(d.Info.RunningWorkflows))
	for _, r := range d.Info.RunningWorkflows {
		fmt.Fprintf(stdout, "    %s\n", r.WorkflowID)
	}
	// Most recent first: what actually fired is the question this command gets opened for.
	acts := d.Info.RecentActions
	for i := len(acts) - 1; i >= 0 && i > len(acts)-6; i-- {
		a := acts[i]
		started := "-"
		if a.StartWorkflowResult != nil {
			started = a.StartWorkflowResult.WorkflowID
		}
		fmt.Fprintf(stdout, "  fired     %s -> %s\n", a.ActualTime.Local().Format("15:04:05"), started)
	}
	if len(acts) == 0 {
		fmt.Fprintf(stdout, "  fired     (nothing yet)\n")
	}
	return nil
}

func scheduleAct(action string, args []string) error {
	fs := flag.NewFlagSet("schedule "+action, flag.ContinueOnError)
	id := fs.String("id", "", "schedule id (required)")
	note := fs.String("note", "", "reason, shown by describe (pause/resume)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	c, err := newScheduleClient()
	if err != nil {
		return fmt.Errorf("dial temporal at %s: %w", temporalAddress(), err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := c.ScheduleClient().GetHandle(ctx, *id)
	switch action {
	case "delete":
		// Deleting a schedule does NOT stop runs it already started — they are independent
		// executions. Say so, because "I deleted the schedule" reads as "I stopped the work".
		if err := h.Delete(ctx); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "deleted schedule %s\n", *id)
		fmt.Fprintf(stdout, "  runs it already started keep going — `kontra runs list` to see them\n")
	case "pause":
		if err := h.Pause(ctx, client.SchedulePauseOptions{Note: *note}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "paused %s\n", *id)
	case "resume":
		if err := h.Unpause(ctx, client.ScheduleUnpauseOptions{Note: *note}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "resumed %s\n", *id)
	case "trigger":
		// Manual firing obeys the schedule's own overlap policy, so triggering a SKIP schedule
		// while its run is alive is a no-op rather than a duplicate.
		if err := h.Trigger(ctx, client.ScheduleTriggerOptions{}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "triggered %s (subject to its overlap policy)\n", *id)
	}
	return nil
}

package app_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// dumps holds, per test, what eventually prints when a condition never holds: a stall then shows the state it stalled
// in (each run's desired and observed state, the node's connection, the lease, pending node commands, human
// notifications) instead of only "timed out".
var dumps sync.Map // *testing.T → *[]func() string

// onTimeout registers a state dump for eventually to print if t times out waiting.
func onTimeout(t *testing.T, dump func() string) {
	t.Helper()
	v, loaded := dumps.LoadOrStore(t, &[]func() string{dump})
	if loaded {
		l := v.(*[]func() string)
		*l = append(*l, dump)
		return
	}
	t.Cleanup(func() { dumps.Delete(t) })
}

// progresses holds, per test, a fingerprint function of the state a wait is waiting on to advance (see
// eventuallyProgress). Tests without one wait on a plain stall window.
var progresses sync.Map // *testing.T → func() string

// onProgress registers the progress fingerprint eventually uses for t.
func onProgress(t *testing.T, fp func() string) {
	t.Helper()
	progresses.Store(t, fp)
	t.Cleanup(func() { progresses.Delete(t) })
}

// eventuallyProgress is a progress-based wait: it polls cond and gives up only when the test's progress fingerprint has
// not changed for stall (or, without a registered fingerprint, after stall, like a plain timeout). A slow system that
// keeps advancing (the single SQLite writer queue under -race and load) is waited for; a stalled one (a lost wake, a
// stuck run) is still cut after stall, with the state dump. maxWait bounds the whole wait regardless, so a fingerprint
// that never stops changing cannot hang the test.
func eventuallyProgress(t *testing.T, what string, stall, maxWait time.Duration, cond func() bool) {
	t.Helper()
	var fp func() string
	if v, ok := progresses.Load(t); ok {
		fp = v.(func() string)
	}
	start := time.Now()
	last, lastChange, sampled := "", time.Now(), time.Now()
	if fp != nil {
		last = fp()
	}
	for !cond() {
		now := time.Now()
		if fp != nil && now.Sub(sampled) >= 250*time.Millisecond { // the fingerprint reads the database: sample it, don't poll it
			sampled = now
			if cur := fp(); cur != last {
				last, lastChange = cur, now
			}
		}
		if now.Sub(lastChange) > stall {
			timedOut(t, what+", no progress", stall)
		}
		if now.Sub(start) > maxWait {
			timedOut(t, what+", still progressing", maxWait)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// timedOut fails t, printing its registered state dumps first.
func timedOut(t *testing.T, what string, d time.Duration) {
	t.Helper()
	if v, ok := dumps.Load(t); ok {
		for _, dump := range *v.(*[]func() string) {
			t.Logf("state when the wait for %s timed out:\n%s", what, dump())
		}
	}
	t.Fatalf("timed out waiting for %s (%s)", what, d)
}

// progress is the fleet env's progress fingerprint: it changes whenever a run changes (a version bump or a change-feed
// entry), a node command changes state or is retried, or a runner terminal appears or changes state. It leaves out
// everything time-driven (lease renewals, heartbeats) so that idling does not look like progress.
func (e *fleetEnv) progress() string {
	c := e.c
	if c == nil {
		return "central is closed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var b strings.Builder
	db := c.Store.ReadDB()
	for _, q := range []string{
		`SELECT count(*) || '/' || coalesce(sum(version), 0) || '/' || coalesce(group_concat(observed), '') FROM runs`,
		`SELECT count(*) FROM changes WHERE resource_type = 'run'`,
		`SELECT count(*) || '/' || coalesce(sum(attempts), 0) || '/' || coalesce(group_concat(state), '') FROM node_commands`,
	} {
		var v string
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			v = err.Error()
		}
		b.WriteString(v + ";")
	}
	if procs, err := e.run.List(ctx, nil, true); err == nil {
		for _, p := range procs {
			fmt.Fprintf(&b, "%s=%s/%s,", p.ID, p.State, p.Status.State)
		}
	}
	return b.String()
}

// dump is the fleet env's state dump: central's view of the node, leases, runs, node commands and human
// notifications, and the fake runner's terminals. It reads central's store directly so that it works whatever state
// the services are in.
func (e *fleetEnv) dump() string {
	var b strings.Builder
	c := e.c
	if c == nil {
		return "central is closed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if nodes, err := c.Fleet.ListNodes(ctx); err != nil {
		fmt.Fprintf(&b, "nodes: %v\n", err)
	} else {
		for _, n := range nodes {
			fmt.Fprintf(&b, "node %s state=%s connected=%v\n", n.ID, n.State, n.Connected)
		}
	}
	db := c.Store.ReadDB()
	for _, q := range []struct{ name, sql string }{
		{"leases", `SELECT space_id, node_id, generation, state, expires_at FROM execution_leases`},
		{"runs", `SELECT id, kind, role, generation, desired, observed, terminal_id, end_reason, error, ended_at FROM runs ORDER BY created_at`},
		{"node commands (last 30)", `SELECT id, kind, run_id, generation, state, reason, attempts FROM node_commands ORDER BY created_at DESC LIMIT 30`},
		{"human notifications", `SELECT id, action_id, target_kind, target_run, state, route_count FROM human_notifications ORDER BY created_at`},
	} {
		fmt.Fprintf(&b, "%s:\n", q.name)
		dumpRows(ctx, &b, db, q.sql)
	}
	if procs, err := e.run.List(ctx, nil, true); err != nil {
		fmt.Fprintf(&b, "runner: %v\n", err)
	} else {
		for _, p := range procs {
			fmt.Fprintf(&b, "terminal %s run=%s state=%s status=%s\n", p.ID, p.Labels["spaceman.run"], p.State, p.Status.State)
		}
	}
	return b.String()
}

func dumpRows(ctx context.Context, b *strings.Builder, db *sql.DB, q string) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		fmt.Fprintf(b, "  %v\n", err)
		return
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Fprintf(b, "  %v\n", err)
			return
		}
		b.WriteString(" ")
		for i, c := range cols {
			fmt.Fprintf(b, " %s=%v", c, vals[i])
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "  %v\n", err)
	}
}

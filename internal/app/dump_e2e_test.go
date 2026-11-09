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

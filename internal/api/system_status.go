package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SystemStatusRules: the instance's status is for administrators (system:read). The version alone stays public in
// get-system-info (SM-OBS-006).
var SystemStatusRules = map[string]Rule{
	"get-system": Global(authz.SystemRead),
}

// SystemSources are the parts of the status central supplies: what it learned at start, live connection state and
// storage health. Every field is optional (nil or zero: reported as unknown).
type SystemSources struct {
	StartedAt time.Time
	// BootID changes at every start of central.
	BootID    string
	PublicURL string
	DataDir   string
	// Schema returns the applied and the newest embedded database migration.
	Schema func(ctx context.Context) (current, latest int64, err error)
	// Nodes returns the number of active nodes and how many of them are connected now.
	Nodes func(ctx context.Context) (active, connected int, err error)
	// Storage is the attention summary's storage section (readiness checks, database health, free space).
	Storage *AttentionSources
	// StoreSizes returns the database and WAL file sizes.
	StoreSizes func() (dbBytes, walBytes int64)
	// LiveStreams returns the number of connected live-event streams (change feed and terminal streams).
	LiveStreams func() int64
}

// SystemStatus is the instance's own status for administrators (SM-OBS-006). It carries no secrets: no key material,
// no key paths and no tokens.
type SystemStatus struct {
	Build         buildinfo.Info `json:"build"`
	StartedAt     time.Time      `json:"started_at"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	BootID        string         `json:"boot_id" doc:"Changes at every start of central"`
	PublicURL     string         `json:"public_url"`
	DataDir       string         `json:"data_dir" doc:"Central's data directory on its host"`
	SchemaVersion SystemSchema   `json:"schema"`
	Protocol      SystemProtocol `json:"protocol"`
	Storage       SystemStorage  `json:"storage"`
	Counts        SystemCounts   `json:"counts"`
}

// SystemSchema is the database schema version: the latest applied goose migration and the newest this binary has.
type SystemSchema struct {
	Current int64 `json:"current"`
	Latest  int64 `json:"latest"`
	// Error is set when the version could not be read ("unavailable").
	Error string `json:"error,omitempty"`
}

// SystemProtocol is the node protocol this central speaks: its major version and the minor range it accepts.
type SystemProtocol struct {
	Major    int `json:"major"`
	MinMinor int `json:"min_minor"`
	MaxMinor int `json:"max_minor"`
}

// SystemStorage is the storage health with the database's file sizes.
type SystemStorage struct {
	AttentionStorage
	DBBytes  int64 `json:"db_bytes"`
	WALBytes int64 `json:"wal_bytes"`
}

// SystemCounts are installation-wide counts.
type SystemCounts struct {
	Spaces         int64 `json:"spaces" doc:"Spaces that are not archived"`
	NodesActive    int64 `json:"nodes_active"`
	NodesConnected int64 `json:"nodes_connected"`
	RunsRunning    int64 `json:"runs_running" doc:"Runs starting, running or stopping"`
	HumanPending   int64 `json:"human_pending" doc:"Open and in-progress human actions"`
	LiveStreams    int64 `json:"live_streams" doc:"Connected live-event streams (change feed, terminals, recordings)"`
}

// RegisterSystemStatus registers get-system.
func RegisterSystemStatus(src *SystemSources) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-system", Method: http.MethodGet, Path: "/system",
			Summary: "This instance's status", Tags: []string{"system"},
			Description: "Version and build, start time and uptime, boot ID, database schema version, the node protocol range, storage health " +
				"and installation-wide counts. For administrators (system:read); the version alone is public in get-system-info. " +
				"It contains no key material, key paths or tokens."},
			func(ctx context.Context, _ *struct{}) (*struct{ Body SystemStatus }, error) {
				out, err := d.systemStatus(ctx, src)
				if err != nil {
					return nil, d.fail(ctx, "system.get", err)
				}
				return &struct{ Body SystemStatus }{Body: *out}, nil
			})
	}
}

func (d *Deps) systemStatus(ctx context.Context, src *SystemSources) (*SystemStatus, error) {
	if src == nil {
		src = &SystemSources{}
	}
	out := &SystemStatus{Build: buildinfo.Get(), StartedAt: src.StartedAt, BootID: src.BootID, PublicURL: src.PublicURL, DataDir: src.DataDir,
		Protocol: SystemProtocol{Major: proto.Major, MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor}}
	if !src.StartedAt.IsZero() {
		out.UptimeSeconds = max(int64(d.now().Sub(src.StartedAt)/time.Second), 0)
	}
	if src.Schema != nil {
		cur, latest, err := src.Schema(ctx)
		if err != nil {
			d.log().WarnContext(ctx, "system status: schema version unavailable", "error", err)
			out.SchemaVersion.Error = "unavailable"
		} else {
			out.SchemaVersion.Current, out.SchemaVersion.Latest = cur, latest
		}
	}
	if src.Storage != nil {
		out.Storage.AttentionStorage = *src.Storage.storage(ctx)
	} else {
		out.Storage.Checks = []AttentionCheck{}
	}
	if src.StoreSizes != nil {
		out.Storage.DBBytes, out.Storage.WALBytes = src.StoreSizes()
	}
	c, err := d.Store.R().SystemCounts(ctx)
	if err != nil {
		return nil, err
	}
	out.Counts = SystemCounts{Spaces: c.Spaces, RunsRunning: c.RunsRunning, HumanPending: c.HumanPending}
	if src.Nodes != nil {
		active, connected, err := src.Nodes(ctx)
		if err != nil {
			return nil, err
		}
		out.Counts.NodesActive, out.Counts.NodesConnected = int64(active), int64(connected)
	}
	if src.LiveStreams != nil {
		out.Counts.LiveStreams = src.LiveStreams()
	}
	return out, nil
}

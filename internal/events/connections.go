package events

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Connection is how to reach a Pulse deployment (SM-EVT-003). Its credentials are write-only.
type Connection struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind" enum:"pulse"`
	URL       string    `json:"url"`
	ClientID  string    `json:"client_id" doc:"The Pulse client the credentials belong to (from the JWT tag); consumers are <client_id>_<durable>"`
	HasCA     bool      `json:"has_ca"`
	Stream    string    `json:"stream"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConnectionInput creates or updates a connection. Creds is .creds content; on update an empty value keeps the
// stored credentials.
type ConnectionInput struct {
	Name    string
	URL     string
	Creds   string
	CAPEM   string
	Stream  string
	Enabled bool
}

var reConnName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func connAAD(id string) []byte { return []byte("event_connection:" + id) }

func connFromRow(r db.EventConnection) *Connection {
	return &Connection{ID: r.ID, Name: r.Name, Kind: r.Kind, URL: r.Url, ClientID: r.ClientID, HasCA: r.CaPem != "", Stream: r.Stream,
		Enabled: r.Enabled == 1, Version: r.Version, CreatedAt: fromMicros(r.CreatedAt), UpdatedAt: fromMicros(r.UpdatedAt)}
}

func (in *ConnectionInput) validate(creating bool) (clientID string, err error) {
	v := &apperr.Validation{}
	if !reConnName.MatchString(in.Name) {
		v.Add("name", "lowercase letters, digits and dashes (2-63)")
	}
	u, perr := url.Parse(in.URL)
	if perr != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" || u.User != nil {
		v.Add("url", "must be nats://host:port or tls://host:port, without credentials")
	}
	if in.Stream == "" {
		in.Stream = pulse.DefaultStream
	}
	if creating && strings.TrimSpace(in.Creds) == "" {
		v.Add("creds", "the .creds content issued by Pulse is required")
	}
	if in.Creds != "" {
		id, cerr := pulse.ClientIDFromCreds([]byte(in.Creds))
		if cerr != nil {
			v.Add("creds", "not a Pulse client credential: %v", cerr)
		}
		clientID = id
	}
	if in.CAPEM != "" && !strings.Contains(in.CAPEM, "BEGIN CERTIFICATE") {
		v.Add("ca_pem", "must be a PEM certificate")
	}
	return clientID, v.Err()
}

// CreateConnection stores a connection with sealed credentials.
func (svc *Service) CreateConnection(ctx context.Context, actor audit.Actor, in ConnectionInput) (*Connection, error) {
	clientID, err := in.validate(true)
	if err != nil {
		return nil, err
	}
	id := ids.New(ids.EventConn)
	sealed, err := svc.kr.Seal(keyring.PurposeCredential, connAAD(id), []byte(in.Creds))
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "events.connection_create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetEventConnection(ctx, id); err == nil {
			return apperr.New(apperr.CodeConflict, "connection exists")
		}
		if err := tx.Q.InsertEventConnection(ctx, db.InsertEventConnectionParams{ID: id, Name: in.Name, Kind: "pulse", Url: in.URL, CredsEnc: sealed,
			ClientID: clientID, CaPem: in.CAPEM, Stream: in.Stream, Enabled: b2i(in.Enabled), CreatedAt: tx.NowMicros(), CreatedBy: actor.ID,
			UpdatedAt: tx.NowMicros()}); err != nil {
			if isUnique(err) {
				return apperr.New(apperr.CodeAlreadyExists, "a connection named %q exists", in.Name)
			}
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_connection.created", Target: audit.Target{Type: "event_connection", ID: id},
			Details: map[string]any{"name": in.Name, "url": in.URL, "client_id": clientID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notify()
	return svc.GetConnection(ctx, id)
}

// UpdateConnection changes a connection; empty Creds keeps the stored credentials.
func (svc *Service) UpdateConnection(ctx context.Context, actor audit.Actor, id string, version int64, in ConnectionInput) (*Connection, error) {
	clientID, err := in.validate(false)
	if err != nil {
		return nil, err
	}
	var sealed []byte
	if in.Creds != "" {
		if sealed, err = svc.kr.Seal(keyring.PurposeCredential, connAAD(id), []byte(in.Creds)); err != nil {
			return nil, err
		}
	}
	err = svc.s.W().TxNamed(ctx, "events.connection_update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetEventConnection(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("connection")
		} else if err != nil {
			return err
		}
		if sealed == nil {
			sealed, clientID = cur.CredsEnc, cur.ClientID
		}
		n, err := tx.Q.UpdateEventConnection(ctx, db.UpdateEventConnectionParams{Name: in.Name, Url: in.URL, CredsEnc: sealed, ClientID: clientID,
			CaPem: in.CAPEM, Stream: in.Stream, Enabled: b2i(in.Enabled), Now: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			if isUnique(err) {
				return apperr.New(apperr.CodeAlreadyExists, "a connection named %q exists", in.Name)
			}
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the connection changed since version %d", version)
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_connection.updated", Target: audit.Target{Type: "event_connection", ID: id},
			Details: map[string]any{"name": in.Name, "url": in.URL, "credentials_replaced": in.Creds != "", "enabled": in.Enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notify()
	return svc.GetConnection(ctx, id)
}

// DeleteConnection removes a connection no subscription uses.
func (svc *Service) DeleteConnection(ctx context.Context, actor audit.Actor, id string) error {
	err := svc.s.W().TxNamed(ctx, "events.connection_delete", func(tx *store.Tx) error {
		if _, err := tx.Q.GetEventConnection(ctx, id); errors.Is(err, sql.ErrNoRows) {
			return notFound("connection")
		}
		n, err := tx.Q.DeleteEventConnection(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "subscriptions still use this connection")
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_connection.deleted", Target: audit.Target{Type: "event_connection", ID: id}})
		return err
	})
	if err == nil {
		svc.notify()
	}
	return err
}

// GetConnection returns a connection (never its credentials).
func (svc *Service) GetConnection(ctx context.Context, id string) (*Connection, error) {
	r, err := svc.s.R().GetEventConnection(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("connection")
	} else if err != nil {
		return nil, err
	}
	return connFromRow(r), nil
}

// ListConnections returns every connection.
func (svc *Service) ListConnections(ctx context.Context) ([]*Connection, error) {
	rows, err := svc.s.R().ListEventConnections(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, connFromRow(r))
	}
	return out, nil
}

// credentials opens a connection's sealed credentials (for the ingestion manager only).
func (svc *Service) credentials(r db.EventConnection) ([]byte, error) {
	return svc.kr.Open(keyring.PurposeCredential, connAAD(r.ID), r.CredsEnc)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

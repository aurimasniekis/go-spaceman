package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SettingsOut is the effective global settings with their storage state.
type SettingsOut struct {
	Settings
	Stored    bool       `json:"stored" doc:"false: nothing was ever set (or it was reset), every value is the built-in default"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	Version   int64      `json:"version" doc:"0 while nothing is stored"`
}

// Service reads and writes the settings and keeps the in-memory copy every Telegram component reads (the cache).
// Writes are version-checked (If-Match) and audited; after each commit the cache is refreshed and a
// eventbus.TopicTelegramSettings event is published, so every subscriber picks the change up without a restart.
type Service struct {
	s     *store.Store
	audit *audit.Writer
	bus   *eventbus.Bus
	sub   *eventbus.Subscription
	log   *slog.Logger

	refreshMu sync.Mutex // serializes refreshes, so listeners see changes in order

	mu        sync.RWMutex
	cur       Settings
	curVer    int64
	loaded    bool
	spaces    map[string]Space
	listeners []func(Settings)
}

// New returns the service with the built-in defaults cached; Load (or Run) reads the stored state.
func New(s *store.Store, aw *audit.Writer, bus *eventbus.Bus, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	svc := &Service{s: s, audit: aw, bus: bus, log: log, cur: Defaults(), spaces: map[string]Space{}}
	if bus != nil {
		// Subscribed at construction, so a change committed before Run starts is still seen. Events only say "reload";
		// a full queue drops events but a reload is pending anyway.
		svc.sub = bus.Subscribe(4, eventbus.TopicTelegramSettings)
	}
	return svc
}

// Run reloads the cache whenever a settings change is published, until ctx ends.
func (svc *Service) Run(ctx context.Context) {
	if err := svc.Load(ctx); err != nil && ctx.Err() == nil {
		svc.log.Warn("telegram: loading the settings failed; using the last known values", slog.String("error", err.Error()))
	}
	if svc.sub == nil {
		<-ctx.Done()
		return
	}
	defer svc.sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-svc.sub.C:
			if !ok {
				return
			}
			if err := svc.Load(ctx); err != nil && ctx.Err() == nil {
				svc.log.Warn("telegram: reloading the settings failed; using the last known values", slog.String("error", err.Error()))
			}
		}
	}
}

// Current returns the cached effective global settings.
func (svc *Service) Current() Settings {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.cur
}

// Space returns a space's cached settings (the defaults, so invisible, when it has none).
func (svc *Service) Space(spaceID string) Space {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if s, ok := svc.spaces[spaceID]; ok {
		return s
	}
	return DefaultSpace(spaceID)
}

// VisibleSpaces returns the IDs of the spaces enabled for Telegram, sorted.
func (svc *Service) VisibleSpaces() []string {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	out := make([]string, 0, len(svc.spaces))
	for id, s := range svc.spaces {
		if s.Visible() {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// OnChange registers fn: it is called at once with the current settings, then after every change of the global
// settings, one call at a time, in order. fn must not block for long and must not call back into OnChange.
func (svc *Service) OnChange(fn func(Settings)) {
	svc.refreshMu.Lock()
	defer svc.refreshMu.Unlock()
	svc.mu.Lock()
	svc.listeners = append(svc.listeners, fn)
	cur := svc.cur
	svc.mu.Unlock()
	fn(cur)
}

// Load reads the stored settings into the cache and tells the listeners if the global settings changed.
func (svc *Service) Load(ctx context.Context) error {
	svc.refreshMu.Lock()
	defer svc.refreshMu.Unlock()
	q := svc.s.R()
	cur, ver := Defaults(), int64(0)
	r, err := q.GetTelegramSettings(ctx)
	switch {
	case err == nil:
		cur, ver = svc.effective(r), r.Version
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	rows, err := q.ListSpaceTelegram(ctx)
	if err != nil {
		return err
	}
	spaces := make(map[string]Space, len(rows))
	for _, row := range rows {
		spaces[row.SpaceID] = spaceFromRow(row)
	}
	svc.mu.Lock()
	changed := !svc.loaded || ver != svc.curVer || cur != svc.cur
	svc.cur, svc.curVer, svc.spaces, svc.loaded = cur, ver, spaces, true
	ls := slices.Clone(svc.listeners)
	svc.mu.Unlock()
	if changed {
		for _, fn := range ls {
			fn(cur)
		}
	}
	return nil
}

// effective applies a stored row to the defaults. A row that no longer validates (bounds tightened in a later release)
// falls back to the defaults, logged, rather than stopping Telegram.
func (svc *Service) effective(r db.TelegramSetting) Settings {
	var in Input
	if err := json.Unmarshal([]byte(r.SettingsJson), &in); err != nil {
		svc.log.Warn("telegram: the stored settings are unreadable; using the defaults", slog.String("error", err.Error()))
		return Defaults()
	}
	s := Resolve(in)
	if err := s.Validate(); err != nil {
		svc.log.Warn("telegram: the stored settings are out of bounds; using the defaults", slog.String("error", err.Error()))
		return Defaults()
	}
	return s
}

// publish wakes every cache after the transaction commits.
func (svc *Service) publish(tx *store.Tx, key string) {
	if svc.bus == nil {
		return
	}
	tx.AfterCommit(func() { svc.bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramSettings, Key: key}) })
}

// refreshNow reloads the cache after a write, so the writer's next read and the sender already see the change; a
// failure only delays it until the bus event's reload.
func (svc *Service) refreshNow(ctx context.Context) {
	if err := svc.Load(ctx); err != nil {
		svc.log.Warn("telegram: reloading the settings failed", slog.String("error", err.Error()))
	}
}

func actorRef(a audit.Actor) string {
	if a.Type == audit.ActorRoot {
		return "root"
	}
	return a.Type + ":" + a.ID
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Get returns the stored global settings with their version (the defaults, version 0, when none are stored).
func (svc *Service) Get(ctx context.Context) (*SettingsOut, error) {
	r, err := svc.s.R().GetTelegramSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return &SettingsOut{Settings: Defaults()}, nil
	} else if err != nil {
		return nil, err
	}
	t := time.UnixMicro(r.UpdatedAt).UTC()
	return &SettingsOut{Settings: svc.effective(r), Stored: true, UpdatedAt: &t, UpdatedBy: r.UpdatedBy, Version: r.Version}, nil
}

// Put stores the global settings: created with version 0 (no If-Match) or replaced at version. Only the fields in
// are stored; everything else follows the built-in defaults.
func (svc *Service) Put(ctx context.Context, actor audit.Actor, version int64, in Input) (*SettingsOut, error) {
	if s := Resolve(in); s.Validate() != nil {
		return nil, s.Validate()
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "telegram.settings.put", func(tx *store.Tx) error {
		now, by := tx.NowMicros(), actorRef(actor)
		var n int64
		var err error
		action := "telegram.settings.created"
		if version == 0 {
			n, err = tx.Q.InsertTelegramSettings(ctx, db.InsertTelegramSettingsParams{SettingsJson: string(raw), UpdatedAt: now, UpdatedBy: by})
			if err == nil && n == 0 {
				return apperr.New(apperr.CodeConflict, "the Telegram settings are already stored: replace them with If-Match")
			}
		} else {
			action = "telegram.settings.updated"
			n, err = tx.Q.UpdateTelegramSettings(ctx, db.UpdateTelegramSettingsParams{SettingsJson: string(raw), Now: now, By: by, Version: version})
			if err == nil && n == 0 {
				return svc.versionErr(ctx, tx, version)
			}
		}
		if err != nil {
			return err
		}
		svc.publish(tx, "")
		var details map[string]any
		_ = json.Unmarshal(raw, &details)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "telegram_settings", ID: "telegram-settings"},
			Details: map[string]any{"set": details}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.refreshNow(ctx)
	return svc.Get(ctx)
}

func (svc *Service) versionErr(ctx context.Context, tx *store.Tx, sent int64) error {
	cur, err := tx.Q.GetTelegramSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeNotFound, "no Telegram settings are stored: create them without If-Match")
	} else if err != nil {
		return err
	}
	return apperr.New(apperr.CodeConflict, "the Telegram settings changed (version %d, you sent %d); reload and retry", cur.Version, sent)
}

// Delete removes the stored settings (version-checked): every value goes back to its built-in default.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, version int64) error {
	err := svc.s.W().TxNamed(ctx, "telegram.settings.delete", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteTelegramSettings(ctx, version)
		if err != nil {
			return err
		}
		if n == 0 {
			return svc.versionErr(ctx, tx, version)
		}
		svc.publish(tx, "")
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.settings.deleted", Target: audit.Target{Type: "telegram_settings", ID: "telegram-settings"}})
		return err
	})
	if err != nil {
		return err
	}
	svc.refreshNow(ctx)
	return nil
}

func spaceFromRow(r db.SpaceTelegramSetting) Space {
	ca, ua := time.UnixMicro(r.CreatedAt).UTC(), time.UnixMicro(r.UpdatedAt).UTC()
	return Space{SpaceID: r.SpaceID, Stored: true, Enabled: r.Enabled == 1, ContentLevel: r.ContentLevel, EnforcedApprovals: r.EnforcedApprovals,
		Topics: r.Topics == 1, TerminalWatch: r.TerminalWatch == 1, CreatedAt: &ca, CreatedBy: r.CreatedBy, UpdatedAt: &ua, UpdatedBy: r.UpdatedBy,
		Version: r.Version}
}

var errSpaceNotFound = apperr.New(apperr.CodeNotFound, "space not found")

func (svc *Service) checkSpace(ctx context.Context, q *db.Queries, spaceID string) error {
	if _, err := q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return errSpaceNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// GetSpace returns a space's stored settings (the defaults, version 0, when it has none).
func (svc *Service) GetSpace(ctx context.Context, spaceID string) (*Space, error) {
	q := svc.s.R()
	if err := svc.checkSpace(ctx, q, spaceID); err != nil {
		return nil, err
	}
	r, err := q.GetSpaceTelegram(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		d := DefaultSpace(spaceID)
		return &d, nil
	} else if err != nil {
		return nil, err
	}
	s := spaceFromRow(r)
	return &s, nil
}

// ListSpaces returns every space with stored settings, by space ID.
func (svc *Service) ListSpaces(ctx context.Context) ([]Space, error) {
	rows, err := svc.s.R().ListSpaceTelegram(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Space, 0, len(rows))
	for _, r := range rows {
		out = append(out, spaceFromRow(r))
	}
	return out, nil
}

func spaceVersionErr(exists bool, cur, sent int64) error {
	switch {
	case !exists && sent != 0:
		return apperr.New(apperr.CodeNotFound, "the space has no Telegram settings yet: create them without If-Match")
	case exists && sent == 0:
		return apperr.New(apperr.CodeConflict, "the space has Telegram settings: replace them with If-Match \"%d\"", cur)
	case exists && sent != cur:
		return apperr.New(apperr.CodeConflict, "the space's Telegram settings changed (version %d, you sent %d); reload and retry", cur, sent)
	}
	return nil
}

// PutSpace stores a space's settings: created with version 0 (no If-Match) or replaced at version. Fields left out
// take their defaults.
func (svc *Service) PutSpace(ctx context.Context, actor audit.Actor, spaceID string, version int64, in SpaceInput) (*Space, error) {
	s, err := in.apply(spaceID)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "telegram.space.put", func(tx *store.Tx) error {
		if err := svc.checkSpace(ctx, tx.Q, spaceID); err != nil {
			return err
		}
		cur, err := tx.Q.GetSpaceTelegram(ctx, spaceID)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := spaceVersionErr(exists, cur.Version, version); err != nil {
			return err
		}
		now, by := tx.NowMicros(), actorRef(actor)
		action := "telegram.space.created"
		if version == 0 {
			if _, err := tx.Q.InsertSpaceTelegram(ctx, db.InsertSpaceTelegramParams{SpaceID: spaceID, Enabled: b2i(s.Enabled), ContentLevel: s.ContentLevel,
				EnforcedApprovals: s.EnforcedApprovals, Topics: b2i(s.Topics), TerminalWatch: b2i(s.TerminalWatch), CreatedAt: now, CreatedBy: by,
				UpdatedAt: now, UpdatedBy: by}); err != nil {
				return err
			}
		} else {
			action = "telegram.space.updated"
			if _, err := tx.Q.UpdateSpaceTelegram(ctx, db.UpdateSpaceTelegramParams{Enabled: b2i(s.Enabled), ContentLevel: s.ContentLevel,
				EnforcedApprovals: s.EnforcedApprovals, Topics: b2i(s.Topics), TerminalWatch: b2i(s.TerminalWatch), Now: now, By: by,
				SpaceID: spaceID, Version: version}); err != nil {
				return err
			}
		}
		svc.publish(tx, spaceID)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "space_telegram", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "enabled": s.Enabled, "content_level": s.ContentLevel, "enforced_approvals": s.EnforcedApprovals,
				"topics": s.Topics, "terminal_watch": s.TerminalWatch}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.refreshNow(ctx)
	return svc.GetSpace(ctx, spaceID)
}

// DeleteSpace removes a space's settings (version-checked): Telegram is off for the space again.
func (svc *Service) DeleteSpace(ctx context.Context, actor audit.Actor, spaceID string, version int64) error {
	err := svc.s.W().TxNamed(ctx, "telegram.space.delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetSpaceTelegram(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "the space has no Telegram settings")
		} else if err != nil {
			return err
		}
		if err := spaceVersionErr(true, cur.Version, version); err != nil {
			return err
		}
		if _, err := tx.Q.DeleteSpaceTelegram(ctx, db.DeleteSpaceTelegramParams{SpaceID: spaceID, Version: version}); err != nil {
			return err
		}
		svc.publish(tx, spaceID)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.space.deleted", Target: audit.Target{Type: "space_telegram", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "was_enabled": cur.Enabled == 1}})
		return err
	})
	if err != nil {
		return err
	}
	svc.refreshNow(ctx)
	return nil
}

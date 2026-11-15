package harborpilot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Space setting bounds (ADR-0060 Q2, Q3).
const (
	DefaultTTLSeconds = 86400  // 24 h
	MinTTLSeconds     = 900    // 15 min
	MaxTTLSeconds     = 604800 // 7 days, Harbor Pilot's own maximum for a child
	BudgetProceed     = "proceed"
	BudgetRefuse      = "refuse"
)

// DefaultHarnesses are routed through Harbor Pilot when a space opts in without naming harnesses.
var DefaultHarnesses = []string{harness.KindClaude, harness.KindCodex}

// RunScopes are the scopes of a run's child token: invoke and models, never tokens:mint.
var RunScopes = []string{ScopeInvoke, ScopeModels}

// Associated data of the sealed tokens (ADR-0060 decision 2): a sealed value opens only for the row it was written for.
func parentAAD(connID string) []byte  { return []byte("harborpilot:parent:" + connID) }
func serviceAAD(connID string) []byte { return []byte("harborpilot:sp:" + connID) }

// RunAAD is the associated data of a run's sealed child token.
func RunAAD(runID string) []byte { return []byte("harborpilot:run:" + runID) }

// ParentFingerprint identifies the parent token that minted a child credential, so a revocation knows whether the
// parent it holds is the one that can revoke it: Harbor Pilot answers 404 to any other parent (ADR-0060 decision 5).
// It is a truncated, domain-separated SHA-256 of a random gateway token, so it reveals nothing usable about the token.
func ParentFingerprint(parent string) string {
	h := sha256.Sum256([]byte("spaceman harbor-pilot parent fingerprint\x00" + parent))
	return hex.EncodeToString(h[:8])
}

func fpArg(fp string) sql.NullString { return sql.NullString{String: fp, Valid: fp != ""} }

// Connection is the Harbor Pilot connection as the API shows it: never a token, only whether one is set and its
// version counter.
type Connection struct {
	ID                  string     `json:"id"`
	BaseURL             string     `json:"base_url" doc:"The gateway origin central calls (mint, revoke, budget status)"`
	RunBaseURL          string     `json:"run_base_url" doc:"The origin runs reach the gateway on; empty means base_url"`
	AllowInsecureHTTP   bool       `json:"allow_insecure_http" doc:"Plain http is allowed: tokens then cross the network in clear text"`
	HasParentToken      bool       `json:"has_parent_token"`
	ParentTokenVersion  int64      `json:"parent_token_version" doc:"Incremented each time a parent token is written"`
	HasServiceToken     bool       `json:"has_service_token"`
	ServiceTokenVersion int64      `json:"service_token_version" doc:"Incremented each time a service-principal token is written or removed"`
	BindingID           string     `json:"binding_id" doc:"The parent token's client binding, learnt when the connection was validated"`
	ParentExpiresAt     *time.Time `json:"parent_expires_at,omitempty" doc:"When the parent token expires, if it does"`
	ValidatedAt         time.Time  `json:"validated_at"`
	EnabledSpaces       []string   `json:"enabled_spaces" doc:"Spaces that route runs through Harbor Pilot"`
	FailedRevocations   int64      `json:"failed_revocations" doc:"Run and validation tokens whose revocation failed: each stays valid until it expires unless it, or the parent that minted it, is revoked in Harbor Pilot (see the audit log)"`
	Warnings            []string   `json:"warnings"`
	CreatedAt           time.Time  `json:"created_at"`
	CreatedBy           string     `json:"created_by"`
	UpdatedAt           time.Time  `json:"updated_at"`
	UpdatedBy           string     `json:"updated_by"`
	Version             int64      `json:"version"`
}

// ConnectionInput creates or replaces the connection. Tokens are write-only.
type ConnectionInput struct {
	BaseURL            string `json:"base_url" maxLength:"2048" doc:"The gateway origin, e.g. https://harbor-pilot.example.com (https unless the host is loopback or allow_insecure_http is set)"`
	RunBaseURL         string `json:"run_base_url,omitempty" maxLength:"2048" doc:"The origin runs reach the gateway on, when it differs from base_url. It needs https (no loopback exemption) unless allow_insecure_http is set"`
	AllowInsecureHTTP  bool   `json:"allow_insecure_http,omitempty" doc:"Allow plain http (tests and trusted local networks only: every token then crosses the network in clear text)"`
	ParentToken        string `json:"parent_token,omitempty" maxLength:"512" writeOnly:"true" doc:"A Harbor Pilot gateway credential (hpk_…) with gateway:invoke, gateway:models and tokens:mint. Required on create and when the gateway address changes; otherwise empty keeps the current one. Never returned"`
	ServiceToken       string `json:"service_token,omitempty" maxLength:"512" writeOnly:"true" doc:"A Harbor Pilot service-principal token (hpa_…) with usage.read and config.read, for budget status and project checks. Empty keeps the current one, except when the gateway address changes (then send it again or set remove_service_token). Never returned"`
	RemoveServiceToken bool   `json:"remove_service_token,omitempty" doc:"Remove the service-principal token"`
}

// SpaceSettings is a space's Harbor Pilot opt-in.
type SpaceSettings struct {
	SpaceID                   string     `json:"space_id"`
	Stored                    bool       `json:"stored" doc:"false: the defaults (not opted in)"`
	Enabled                   bool       `json:"enabled"`
	ProjectID                 string     `json:"project_id" doc:"The Harbor Pilot project the space's runs are charged to"`
	Harnesses                 []string   `json:"harnesses" doc:"Harnesses whose runs go through Harbor Pilot; the others keep their direct accounts"`
	TTLSeconds                int64      `json:"ttl_seconds" doc:"Lifetime of a run's token"`
	OnBudgetStatusUnavailable string     `json:"on_budget_status_unavailable" enum:"proceed,refuse" doc:"What a launch does when Harbor Pilot budget status can't be read"`
	AllowedModelAliases       []string   `json:"allowed_model_aliases" doc:"Models runs may use; empty: any model the binding's plan routes"`
	Connected                 bool       `json:"connected" doc:"A Harbor Pilot connection exists"`
	Warnings                  []string   `json:"warnings"`
	UpdatedAt                 *time.Time `json:"updated_at,omitempty"`
	UpdatedBy                 string     `json:"updated_by,omitempty"`
	Version                   int64      `json:"version"`
}

// SpaceInput sets a space's opt-in.
type SpaceInput struct {
	Enabled                   bool     `json:"enabled,omitempty"`
	ProjectID                 string   `json:"project_id,omitempty" maxLength:"64" doc:"The Harbor Pilot project id (required when enabled); it must be the connection's binding project or one of its allowed projects. Setting or changing it needs account:write; empty on an update keeps the stored project"`
	Harnesses                 []string `json:"harnesses,omitempty" maxItems:"16" doc:"Harness kinds routed through Harbor Pilot (any harness it can serve); empty: claude-code and codex"`
	TTLSeconds                int64    `json:"ttl_seconds,omitempty" minimum:"0" maximum:"604800" doc:"Lifetime of a run's token: 900 to 604800 seconds; 0: 86400 (24 h)"`
	OnBudgetStatusUnavailable string   `json:"on_budget_status_unavailable,omitempty" enum:"proceed,refuse," doc:"proceed (default; the gateway still enforces its budgets) or refuse"`
	AllowedModelAliases       []string `json:"allowed_model_aliases,omitempty" maxItems:"64" doc:"Model aliases runs may use; empty: any"`
}

// Options configure the service.
type Options struct {
	// Transport replaces the HTTP transport of Harbor Pilot calls (tests).
	Transport http.RoundTripper
	// Timeout bounds each Harbor Pilot call (default 15 s).
	Timeout time.Duration
	// Log receives failures nobody else sees (nil: discarded).
	Log *slog.Logger
	// Wake is called after a commit that queued credential revocations: it wakes the deadline timer the revoker
	// (Lifecycle.RevokeDue) runs on. Nil: queued revocations wait for the timer's next round.
	Wake func()
}

// Service keeps the connection and the space settings.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	kr      *keyring.Keyring
	o       Options
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, kr *keyring.Keyring, o Options) *Service {
	return &Service{s: s, audit: aw, changes: ch, kr: kr, o: o}
}

var (
	errNoConnection      = apperr.New(apperr.CodeNotFound, "no Harbor Pilot connection is configured")
	errNotConnected      = apperr.New(apperr.CodePrecondition, "no Harbor Pilot connection is configured: an administrator connects Harbor Pilot first")
	errProjectNeedsAdmin = apperr.New(apperr.CodeForbidden, "setting a space's Harbor Pilot project needs account:write")
	reHarness            = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	// Harbor Pilot bound-ref values are 1-256 bytes; its ids are ULID-based (prj_…). Names are refused by the probe.
	reProjectID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	reAlias     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
	reTokenText = regexp.MustCompile(`^[\x21-\x7e]{8,512}$`)
)

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

func (svc *Service) client(baseURL string, insecure bool) (*Client, error) {
	return NewClient(baseURL, ClientOptions{AllowInsecureHTTP: insecure, Transport: svc.o.Transport, Timeout: svc.o.Timeout})
}

// GetConnection returns the connection (not found when none is configured).
func (svc *Service) GetConnection(ctx context.Context) (*Connection, error) {
	r, err := svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoConnection
	} else if err != nil {
		return nil, err
	}
	return svc.connOut(ctx, r)
}

func (svc *Service) connOut(ctx context.Context, r db.HarborPilotConnection) (*Connection, error) {
	enabled, err := svc.s.R().ListEnabledSpaceHarborPilot(ctx)
	if err != nil {
		return nil, err
	}
	c := &Connection{ID: r.ID, BaseURL: r.BaseUrl, RunBaseURL: r.RunBaseUrl, AllowInsecureHTTP: r.AllowInsecureHttp == 1, HasParentToken: len(r.ParentTokenEnc) > 0,
		ParentTokenVersion: r.ParentTokenVersion, HasServiceToken: len(r.ServiceTokenEnc) > 0, ServiceTokenVersion: r.ServiceTokenVersion, BindingID: r.BindingID,
		ValidatedAt: time.UnixMicro(r.ValidatedAt).UTC(), EnabledSpaces: make([]string, 0, len(enabled)), Warnings: []string{},
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), UpdatedBy: r.UpdatedBy, Version: r.Version}
	if r.ParentExpiresAt.Valid {
		t := time.UnixMicro(r.ParentExpiresAt.Int64).UTC()
		c.ParentExpiresAt = &t
	}
	for _, e := range enabled {
		c.EnabledSpaces = append(c.EnabledSpaces, e.SpaceID)
	}
	if c.FailedRevocations, err = svc.s.R().CountFailedGatewayRevocations(ctx, svc.s.Now().UnixMicro()); err != nil {
		return nil, err
	}
	if c.FailedRevocations > 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf("%d token revocations failed: those tokens stay valid until they expire unless they, or the parent that "+
			"minted them, are revoked in Harbor Pilot (the audit log lists them as harbor_pilot.revocation.failed).", c.FailedRevocations))
	}
	if c.AllowInsecureHTTP {
		c.Warnings = append(c.Warnings, "Insecure HTTP is allowed: the parent token and every run's token cross the network in clear text.")
	}
	if !c.HasServiceToken {
		c.Warnings = append(c.Warnings, "No service-principal token: Harbor Pilot budget status, project checks and per-run usage are unavailable.")
	}
	now := svc.s.Now()
	if c.ParentExpiresAt != nil {
		if !c.ParentExpiresAt.After(now) {
			c.Warnings = append(c.Warnings, "The parent token has expired: runs can't get tokens until it is replaced.")
		} else {
			for _, e := range enabled {
				if now.Add(time.Duration(e.TtlSeconds) * time.Second).After(*c.ParentExpiresAt) {
					c.Warnings = append(c.Warnings, parentExpiryWarning(*c.ParentExpiresAt, e.SpaceID, e.TtlSeconds))
				}
			}
		}
	}
	return c, nil
}

func parentExpiryWarning(at time.Time, spaceID string, ttl int64) string {
	return fmt.Sprintf("The parent token expires at %s, sooner than space %s's token lifetime (%s): Harbor Pilot shortens its runs' tokens to the parent's expiry.",
		at.Format(time.RFC3339), spaceID, time.Duration(ttl)*time.Second)
}

// validation is what a connection probe learnt.
type validation struct {
	bindingID     string
	parentExpires *time.Time
	unrevoked     *Credential // the probe credential could not be revoked
	parentFP      string      // ParentFingerprint of the parent that minted it
}

// The probe child lives for Harbor Pilot's maximum, so its expires_at shows whether the parent expires sooner (Harbor
// Pilot clamps a child's TTL to its parent's expiry). Its token is discarded at once and the credential revoked.
const probeTTL = MaxTTLSeconds

// probeParent validates a parent token by minting a child with check_budget sent explicitly (a gateway older than
// 0.1.3 refuses the field) and revoking it.
func (svc *Service) probeParent(ctx context.Context, c *Client, parent string, boundRefs map[string]string, ttl int64) (*validation, error) {
	cred, _, err := c.Mint(ctx, parent, MintRequest{Name: "spaceman-validation", Scopes: RunScopes, TTLSeconds: ttl, BoundRefs: boundRefs, CheckBudget: false})
	if err != nil {
		return nil, err
	}
	v := &validation{bindingID: cred.BindingID, parentFP: ParentFingerprint(parent)}
	if !cred.ExpiresAt.IsZero() && cred.ExpiresAt.Before(svc.s.Now().Add(time.Duration(ttl)*time.Second-5*time.Minute)) {
		t := cred.ExpiresAt.UTC()
		v.parentExpires = &t
	}
	if err := c.Revoke(ctx, parent, cred.ID); err != nil {
		v.unrevoked = cred
	}
	return v, nil
}

// queueRevocation records a credential that could not be revoked at once in the durable revocation queue, with the
// fingerprint of the parent that minted it.
func (svc *Service) queueRevocation(ctx context.Context, cred *Credential, reason, parentFP string) error {
	now := svc.s.Now()
	exp := cred.ExpiresAt
	if exp.IsZero() {
		exp = now.Add(probeTTL * time.Second)
	}
	err := svc.s.W().TxNamed(ctx, "harborpilot.queue_revocation", func(tx *store.Tx) error {
		return tx.Q.InsertGatewayRevocation(ctx, db.InsertGatewayRevocationParams{CredentialID: cred.ID, Reason: reason, ExpiresAt: exp.UnixMicro(),
			NextAttemptAt: now.UnixMicro(), RequestedAt: now.UnixMicro(), ParentFp: fpArg(parentFP)})
	})
	if err == nil {
		svc.wake()
	}
	return err
}

func (svc *Service) wake() {
	if svc.o.Wake != nil {
		svc.o.Wake()
	}
}

// unrevokedWarning queues a validation credential that could not be revoked and says what became of it. When even the
// queue can't take it, the credential stays valid until it expires: that is logged and said.
func (svc *Service) unrevokedWarning(ctx context.Context, val *validation, reason string) string {
	cred := val.unrevoked
	if err := svc.queueRevocation(ctx, cred, reason, val.parentFP); err != nil {
		svc.log().Error("harbor pilot validation credential neither revoked nor queued", "credential_id", cred.ID, "reason", reason, "err", err)
		return "The validation credential " + cred.ID + " could not be revoked, nor queued for revocation; it stays valid until " +
			cred.ExpiresAt.UTC().Format(time.RFC3339) + " unless it is revoked in Harbor Pilot."
	}
	return "The validation credential " + cred.ID + " could not be revoked at once; it is queued for revocation."
}

func (svc *Service) log() *slog.Logger {
	if svc.o.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return svc.o.Log
}

// hpField turns a Harbor Pilot failure during validation into an API error: refusals of what the caller sent become
// field errors, an unreachable or failing gateway is unavailable.
func hpField(field string, err error) error {
	var he *Error
	if !errors.As(err, &he) {
		return err
	}
	switch he.Class {
	case ClassTransport, ClassUpstream, ClassRateLimited:
		return apperr.New(apperr.CodeUnavailable, "Harbor Pilot could not validate the connection: %s", he.Reason())
	case ClassTooOld:
		field = "base_url"
	}
	v := &apperr.Validation{}
	v.Add(field, "%s", he.Reason())
	return v.Err()
}

const errReenter = "re-enter the tokens when the gateway URL changes: stored tokens are never sent to a new address"

// addressChanged reports whether a connection write moves the gateway: another base_url origin (scheme, host, port or
// path), another origin runs reach, or plain http newly allowed. URLs are compared as ParseBaseURL normalised them.
func addressChanged(cur db.HarborPilotConnection, base, runBase string, insecure bool) bool {
	run, curRun := runBase, cur.RunBaseUrl
	if run == "" {
		run = base
	}
	if curRun == "" {
		curRun = cur.BaseUrl
	}
	return base != cur.BaseUrl || run != curRun || (insecure && cur.AllowInsecureHttp == 0)
}

func checkToken(v *apperr.Validation, field, tok, prefix, other, kind string) {
	switch {
	case !reTokenText.MatchString(tok):
		v.Add(field, "must be 8-512 printable characters without spaces")
	case strings.HasPrefix(tok, other):
		v.Add(field, "is a %s token (%s…); this field needs a %s", map[string]string{"hpk_": "gateway", "hpa_": "service-principal"}[other], other, kind)
	case !strings.HasPrefix(tok, prefix):
		v.Add(field, "must be a %s (%s…)", kind, prefix)
	}
}

// PutConnection creates the connection (version 0) or replaces it (version-checked). Every write is validated against
// Harbor Pilot first, outside any transaction: a probe mint with the parent token (it must hold tokens:mint, belong to
// a 0.1.3 or later gateway, and is revoked at once), and, with a service-principal token, budget status (usage.read)
// and a project read (config.read).
func (svc *Service) PutConnection(ctx context.Context, actor audit.Actor, version int64, in ConnectionInput) (*Connection, error) {
	if svc.kr == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "the Harbor Pilot connection needs the keyring")
	}
	cur, err := svc.s.R().GetHarborPilotConnection(ctx)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	switch {
	case !exists && version != 0:
		return nil, errNoConnection
	case exists && version == 0:
		return nil, apperr.New(apperr.CodeConflict, "a Harbor Pilot connection exists: replace it with If-Match \"%d\"", cur.Version)
	case exists && version != cur.Version:
		return nil, apperr.New(apperr.CodeConflict, "the connection changed (version %d, you sent %d); reload and retry", cur.Version, version)
	}

	v := &apperr.Validation{}
	base, err := ParseBaseURL(in.BaseURL, in.AllowInsecureHTTP, true)
	if err != nil {
		v.Add("base_url", "%s", err.Error())
	}
	runURL := strings.TrimSpace(in.RunBaseURL)
	runField := "run_base_url"
	if runURL == "" && base != nil {
		runURL, runField = base.String(), "base_url"
	}
	if runURL != "" {
		u, err := ParseBaseURL(runURL, in.AllowInsecureHTTP, false)
		switch {
		case err != nil && runField == "base_url":
			v.Add("run_base_url", "runs reach the gateway on base_url, which %s; set run_base_url to the https origin runs reach", err.Error())
		case err != nil:
			v.Add("run_base_url", "%s", err.Error())
		case in.RunBaseURL != "":
			in.RunBaseURL = u.String()
		}
	}
	if in.ParentToken == "" && !exists {
		v.Add("parent_token", "is required")
	} else if in.ParentToken != "" {
		checkToken(v, "parent_token", in.ParentToken, "hpk_", "hpa_", "gateway credential with tokens:mint")
	}
	if in.ServiceToken != "" {
		checkToken(v, "service_token", in.ServiceToken, "hpa_", "hpk_", "service-principal token")
		if in.RemoveServiceToken {
			v.Add("remove_service_token", "can't be set together with service_token")
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	// The stored tokens are only ever sent to the address they were entered for: a changed gateway address (or newly
	// allowed plain http) needs them entered again, so whoever can write the connection can't redirect them elsewhere.
	if exists && addressChanged(cur, base.String(), in.RunBaseURL, in.AllowInsecureHTTP) {
		if in.ParentToken == "" {
			v.Add("parent_token", "%s", errReenter)
		}
		if len(cur.ServiceTokenEnc) > 0 && in.ServiceToken == "" && !in.RemoveServiceToken {
			v.Add("service_token", "%s (or set remove_service_token)", errReenter)
		}
		if err := v.Err(); err != nil {
			return nil, err
		}
	}

	id := cur.ID
	if !exists {
		id = ids.New(ids.HarborPilot)
	}
	parent := in.ParentToken
	if parent == "" {
		pt, err := svc.kr.Open(keyring.PurposeCredential, parentAAD(id), cur.ParentTokenEnc)
		if err != nil {
			return nil, fmt.Errorf("harborpilot: open the parent token: %w", err)
		}
		parent = string(pt)
	}
	service := in.ServiceToken
	if service == "" && !in.RemoveServiceToken && exists && len(cur.ServiceTokenEnc) > 0 {
		st, err := svc.kr.Open(keyring.PurposeCredential, serviceAAD(id), cur.ServiceTokenEnc)
		if err != nil {
			return nil, fmt.Errorf("harborpilot: open the service-principal token: %w", err)
		}
		service = string(st)
	}

	c, err := svc.client(base.String(), in.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	val, err := svc.probeParent(ctx, c, parent, nil, probeTTL)
	if err != nil {
		return nil, hpField("parent_token", err)
	}
	var warnings []string
	if val.unrevoked != nil {
		warnings = append(warnings, svc.unrevokedWarning(ctx, val, "connection validation"))
	}
	if service != "" {
		if _, err := c.BudgetStatus(ctx, service); err != nil {
			if IsClass(err, ClassForbidden) {
				return nil, validationErr("service_token", "the service-principal token lacks usage.read")
			}
			return nil, hpField("service_token", err)
		}
		// config.read: reading a project that cannot exist answers 404 with it and 403 without.
		if _, err := c.GetProject(ctx, service, "spaceman-validation-probe"); err != nil && !IsClass(err, ClassNotFound) {
			if IsClass(err, ClassForbidden) {
				return nil, validationErr("service_token", "the service-principal token lacks config.read")
			}
			return nil, hpField("service_token", err)
		}
	}

	// A new parent can't revoke the children of the old one (Harbor Pilot answers 404), so every credential the old
	// parent minted that may still be valid (live run tokens and pending revocations) is revoked now, with the old
	// parent at its old address, before it is replaced (ADR-0060 decision 5). The connection write's transaction checks
	// that nothing of the old parent's was added meanwhile (a run token minted between the list and the commit) and
	// revokes those in another round; a mint stored after the commit sees the parent changed and revokes its own
	// credential (Lifecycle.Token).
	//
	// If the connection write then fails (a version conflict, a seal error), the old parent stays while the run tokens
	// are already revoked: those runs have no model access until they are started again. That fails safe: no token
	// stays valid that SpaceMan believes revoked.
	var rotated []rotatedChild
	oldParent, oldFP := "", ""
	if exists && in.ParentToken != "" {
		if old, err := svc.open(parentAAD(id), cur.ParentTokenEnc, "parent"); err == nil && old != in.ParentToken {
			oldParent, oldFP = old, ParentFingerprint(old)
		}
	}
	revokeOld := func(skip map[string]bool) error {
		if oldParent == "" {
			return nil
		}
		more, err := svc.revokeChildren(ctx, cur, oldParent, oldFP, skip)
		rotated = append(rotated, more...)
		return err
	}
	if err := revokeOld(nil); err != nil {
		return nil, err
	}

	sealedParent, err := svc.kr.Seal(keyring.PurposeCredential, parentAAD(id), []byte(parent))
	if err != nil {
		return nil, err
	}
	var sealedService []byte
	if service != "" {
		if sealedService, err = svc.kr.Seal(keyring.PurposeCredential, serviceAAD(id), []byte(service)); err != nil {
			return nil, err
		}
	}
	var expires sql.NullInt64
	if val.parentExpires != nil {
		expires = sql.NullInt64{Int64: val.parentExpires.UnixMicro(), Valid: true}
	}
	for round := 1; ; round++ {
		var missed bool
		err = svc.s.W().TxNamed(ctx, "harborpilot.put_connection", func(tx *store.Tx) error {
			missed = false
			if oldParent != "" {
				handled := rotatedSet(rotated)
				left, err := tx.Q.ListParentGatewayCredentials(ctx, fpArg(oldFP))
				if err != nil {
					return err
				}
				for _, r := range left {
					if !handled[r.CredentialID] {
						missed = true
						return errRotationRace
					}
				}
			}
			return svc.putConnectionTx(ctx, tx, actor, version, exists, id, in, base.String(), val, sealedParent, sealedService, expires, rotated)
		})
		if !missed || round >= rotationRounds {
			break
		}
		if err := revokeOld(rotatedSet(rotated)); err != nil {
			return nil, err
		}
	}
	if errors.Is(err, errRotationRace) {
		return nil, apperr.New(apperr.CodeUnavailable, "run tokens kept being minted with the old parent token while it was being replaced; retry")
	}
	if err != nil {
		return nil, err
	}
	if len(rotated) > 0 {
		svc.wake() // the deadline timer re-reads the queue, failed revocations included
	}
	return svc.rotationOut(ctx, warnings, rotated)
}

// rotationRounds bounds the revoke-and-commit rounds of a parent rotation that keeps finding new credentials of the
// old parent.
const rotationRounds = 3

var errRotationRace = errors.New("harborpilot: a credential of the old parent was added during the rotation")

func rotatedSet(rs []rotatedChild) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[r.cred.CredentialID] = true
	}
	return m
}

// putConnectionTx writes the connection inside PutConnection's transaction and records the rotated children.
func (svc *Service) putConnectionTx(ctx context.Context, tx *store.Tx, actor audit.Actor, version int64, exists bool, id string, in ConnectionInput, base string,
	val *validation, sealedParent, sealedService []byte, expires sql.NullInt64, rotated []rotatedChild) error {
	now := tx.NowMicros()
	row, err := tx.Q.GetHarborPilotConnection(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if exists {
			return errNoConnection
		}
	case err != nil:
		return err
	case !exists:
		return apperr.New(apperr.CodeConflict, "a Harbor Pilot connection was created meanwhile; reload and retry")
	}
	parentVersion, serviceVersion := int64(1), int64(0)
	if sealedService != nil {
		serviceVersion = 1
	}
	action := "created"
	if exists {
		action = "updated"
		parentVersion, serviceVersion = row.ParentTokenVersion, row.ServiceTokenVersion
		if in.ParentToken != "" {
			parentVersion++
		}
		if in.ServiceToken != "" || (in.RemoveServiceToken && len(row.ServiceTokenEnc) > 0) {
			serviceVersion++
		}
		if row.BindingID != "" && row.BindingID != val.bindingID {
			enabled, err := tx.Q.ListEnabledSpaceHarborPilot(ctx)
			if err != nil {
				return err
			}
			if len(enabled) > 0 {
				return apperr.New(apperr.CodePrecondition, "the parent token belongs to another client binding (%s, was %s) while %d spaces use Harbor Pilot; "+
					"use a parent of the same binding, or disable those spaces first", val.bindingID, row.BindingID, len(enabled))
			}
		}
		n, err := tx.Q.UpdateHarborPilotConnection(ctx, db.UpdateHarborPilotConnectionParams{BaseUrl: base, RunBaseUrl: in.RunBaseURL,
			AllowInsecureHttp: b2i(in.AllowInsecureHTTP), ParentTokenEnc: sealedParent, ParentTokenVersion: parentVersion, ServiceTokenEnc: sealedService,
			ServiceTokenVersion: serviceVersion, BindingID: val.bindingID, ParentExpiresAt: expires, Now: now, UpdatedBy: actorRef(actor), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the connection changed (version %d, you sent %d); reload and retry", row.Version, version)
		}
	} else if err := tx.Q.InsertHarborPilotConnection(ctx, db.InsertHarborPilotConnectionParams{ID: id, BaseUrl: base, RunBaseUrl: in.RunBaseURL,
		AllowInsecureHttp: b2i(in.AllowInsecureHTTP), ParentTokenEnc: sealedParent, ServiceTokenEnc: sealedService, ServiceTokenVersion: serviceVersion,
		BindingID: val.bindingID, ParentExpiresAt: expires, ValidatedAt: now, CreatedAt: now, CreatedBy: actorRef(actor), UpdatedAt: now, UpdatedBy: actorRef(actor)}); err != nil {
		if store.IsConstraint(err) {
			return apperr.New(apperr.CodeConflict, "a Harbor Pilot connection was created meanwhile; reload and retry")
		}
		return err
	}
	for _, r := range rotated {
		if err := r.record(ctx, tx, svc.audit); err != nil {
			return err
		}
	}
	if err := svc.changes.Record(ctx, tx, "", "harbor_pilot_connection", id, action, version+1); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "harbor_pilot.connection." + action, Target: audit.Target{Type: "harbor_pilot_connection", ID: id},
		Details: map[string]any{"base_url": base, "run_base_url": in.RunBaseURL, "allow_insecure_http": in.AllowInsecureHTTP,
			"parent_token_changed": in.ParentToken != "", "parent_token_version": parentVersion, "service_token_changed": in.ServiceToken != "" || in.RemoveServiceToken,
			"has_service_token": sealedService != nil, "service_token_version": serviceVersion, "binding_id": val.bindingID,
			"run_tokens_revoked": len(rotated)}})
	return err
}

// rotationOut is the connection a write returns, with what became of the old parent's credentials.
func (svc *Service) rotationOut(ctx context.Context, warnings []string, rotated []rotatedChild) (*Connection, error) {
	out, err := svc.GetConnection(ctx)
	if err != nil {
		return nil, err
	}
	out.Warnings = append(out.Warnings, warnings...)
	if len(rotated) > 0 {
		failed := 0
		for _, r := range rotated {
			if r.state == "failed" {
				failed++
			}
		}
		out.Warnings = append(out.Warnings, fmt.Sprintf("The parent token changed: %d Harbor Pilot tokens minted by the old parent (live run tokens and "+
			"pending revocations) were revoked with it (the new one can't revoke them); runs whose token was revoked have no model access until they are "+
			"started again.", len(rotated)))
		if failed > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%d of those tokens could not be revoked; revoke the old parent in Harbor Pilot, which ends its children.", failed))
		}
	}
	return out, nil
}

// rotatedChild is a credential of the old parent (a live run token or a pending revocation) revoked because the parent
// token is being replaced.
type rotatedChild struct {
	cred       db.ListParentGatewayCredentialsRow
	state, msg string
}

// revokeChildren revokes, with the old parent at the connection's stored address (outside any transaction), every
// credential the old parent minted that may still be valid, except those in skip; record stores the outcomes in the
// connection write's transaction. A credential of an unknown parent is tried too, but only a 204 counts for it.
func (svc *Service) revokeChildren(ctx context.Context, cur db.HarborPilotConnection, oldParent, oldFP string, skip map[string]bool) ([]rotatedChild, error) {
	all, err := svc.s.R().ListParentGatewayCredentials(ctx, fpArg(oldFP))
	if err != nil {
		return nil, err
	}
	var creds []db.ListParentGatewayCredentialsRow
	for _, cr := range all {
		if !skip[cr.CredentialID] {
			creds = append(creds, cr)
		}
	}
	if len(creds) == 0 {
		return nil, nil
	}
	c, err := svc.client(cur.BaseUrl, cur.AllowInsecureHttp == 1)
	out := make([]rotatedChild, 0, len(creds))
	for _, cred := range creds {
		r := rotatedChild{cred: cred}
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, revokeTimeout)
			rerr := c.Revoke(cctx, oldParent, cred.CredentialID)
			cancel()
			r.state, r.msg = revokeOutcome(rerr, cred.Fp == oldFP)
		} else {
			r.msg = "the old address is not usable"
		}
		// The old parent is about to be replaced: nothing can retry this revocation later.
		if r.state == "" {
			r.state, r.msg = "failed", "the parent token was replaced before this credential could be revoked: "+r.msg
		}
		out = append(out, r)
	}
	return out, nil
}

// record stores the outcome of a rotated credential: a live run token is dropped and its revocation recorded, and a
// pending revocation is settled.
func (r rotatedChild) record(ctx context.Context, tx *store.Tx, aw *audit.Writer) error {
	if _, err := tx.Q.ClearRunGatewayTokenFor(ctx, db.ClearRunGatewayTokenForParams{RunID: r.cred.RunID, CredentialID: r.cred.CredentialID}); err != nil {
		return err
	}
	if err := tx.Q.InsertGatewayRevocation(ctx, db.InsertGatewayRevocationParams{CredentialID: r.cred.CredentialID, RunID: r.cred.RunID, SpaceID: r.cred.SpaceID,
		Reason: "parent_rotated", ExpiresAt: r.cred.ExpiresAt, NextAttemptAt: tx.NowMicros(), RequestedAt: tx.NowMicros(), ParentFp: fpArg(r.cred.Fp)}); err != nil {
		return err
	}
	n, err := tx.Q.SetGatewayRevocationDone(ctx, db.SetGatewayRevocationDoneParams{State: r.state, LastError: bounded(r.msg, 500),
		Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, CredentialID: r.cred.CredentialID})
	if err != nil || n == 0 || r.state != "failed" {
		return err
	}
	_, err = aw.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "harbor_pilot.revocation.failed", Target: audit.Target{Type: "harbor_pilot_credential", ID: r.cred.CredentialID},
		Details: map[string]any{"run_id": r.cred.RunID, "space_id": r.cred.SpaceID, "reason": "parent_rotated", "error": bounded(r.msg, 500)}})
	return err
}

// DeleteConnection removes the connection (version-checked). It is refused while any space routes runs through Harbor
// Pilot, or while unexpired tokens (run or validation) are live or pending revocation (their revocation needs the
// parent token).
func (svc *Service) DeleteConnection(ctx context.Context, actor audit.Actor, version int64) error {
	return svc.s.W().TxNamed(ctx, "harborpilot.delete_connection", func(tx *store.Tx) error {
		cur, err := tx.Q.GetHarborPilotConnection(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoConnection
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodeConflict, "the connection changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		enabled, err := tx.Q.ListEnabledSpaceHarborPilot(ctx)
		if err != nil {
			return err
		}
		if len(enabled) > 0 {
			return apperr.New(apperr.CodePrecondition, "%d spaces route runs through Harbor Pilot (first: %s); disable Harbor Pilot in them first", len(enabled), enabled[0].SpaceID)
		}
		// Expired credentials no longer matter: Harbor Pilot refuses them anyway.
		live, err := tx.Q.CountLiveRunGatewayCredentials(ctx, tx.NowMicros())
		if err != nil {
			return err
		}
		pending, err := tx.Q.CountPendingGatewayRevocations(ctx, tx.NowMicros())
		if err != nil {
			return err
		}
		if live+pending > 0 {
			return apperr.New(apperr.CodePrecondition, "%d unexpired tokens (run or validation) are live or waiting to be revoked, which needs the parent token; "+
				"retry once they are revoked or expired", live+pending)
		}
		n, err := tx.Q.DeleteHarborPilotConnection(ctx, db.DeleteHarborPilotConnectionParams{ID: cur.ID, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the connection changed; reload and retry")
		}
		if err := svc.changes.Record(ctx, tx, "", "harbor_pilot_connection", cur.ID, "deleted", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "harbor_pilot.connection.deleted", Target: audit.Target{Type: "harbor_pilot_connection", ID: cur.ID},
			Details: map[string]any{"base_url": cur.BaseUrl, "binding_id": cur.BindingID}})
		return err
	})
}

func (svc *Service) open(aad, sealed []byte, what string) (string, error) {
	if svc.kr == nil {
		return "", errors.New("harborpilot: no keyring")
	}
	pt, err := svc.kr.Open(keyring.PurposeCredential, aad, sealed)
	if err != nil {
		return "", fmt.Errorf("harborpilot: open the %s token: %w", what, err)
	}
	return string(pt), nil
}

// ParentToken opens the parent token (central's run-token lifecycle only; it never leaves central).
func (svc *Service) ParentToken(ctx context.Context) (string, error) {
	r, err := svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoConnection
	} else if err != nil {
		return "", err
	}
	if svc.kr == nil {
		return "", errors.New("harborpilot: no keyring")
	}
	pt, err := svc.kr.Open(keyring.PurposeCredential, parentAAD(r.ID), r.ParentTokenEnc)
	if err != nil {
		return "", fmt.Errorf("harborpilot: open the parent token: %w", err)
	}
	return string(pt), nil
}

// ServiceToken opens the service-principal token ("" when none is set).
func (svc *Service) ServiceToken(ctx context.Context) (string, error) {
	r, err := svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoConnection
	} else if err != nil {
		return "", err
	}
	if len(r.ServiceTokenEnc) == 0 {
		return "", nil
	}
	if svc.kr == nil {
		return "", errors.New("harborpilot: no keyring")
	}
	st, err := svc.kr.Open(keyring.PurposeCredential, serviceAAD(r.ID), r.ServiceTokenEnc)
	if err != nil {
		return "", fmt.Errorf("harborpilot: open the service-principal token: %w", err)
	}
	return string(st), nil
}

// GetSpace returns a space's settings (the defaults when it never opted in).
func (svc *Service) GetSpace(ctx context.Context, spaceID string) (*SpaceSettings, error) {
	q := svc.s.R()
	if _, err := q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return nil, err
	}
	conn, err := q.GetHarborPilotConnection(ctx)
	connected := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out := &SpaceSettings{SpaceID: spaceID, Harnesses: slices.Clone(DefaultHarnesses), TTLSeconds: DefaultTTLSeconds, OnBudgetStatusUnavailable: BudgetProceed,
		AllowedModelAliases: []string{}, Connected: connected, Warnings: []string{}}
	r, err := q.GetSpaceHarborPilot(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	out.Stored, out.Enabled, out.ProjectID, out.TTLSeconds, out.OnBudgetStatusUnavailable = true, r.Enabled == 1, r.ProjectID, r.TtlSeconds, r.OnBudgetStatusUnavailable
	_ = json.Unmarshal([]byte(r.HarnessesJson), &out.Harnesses)
	_ = json.Unmarshal([]byte(r.AllowedModelAliasesJson), &out.AllowedModelAliases)
	if out.Harnesses == nil {
		out.Harnesses = []string{}
	}
	if out.AllowedModelAliases == nil {
		out.AllowedModelAliases = []string{}
	}
	t := time.UnixMicro(r.UpdatedAt).UTC()
	out.UpdatedAt, out.UpdatedBy, out.Version = &t, r.UpdatedBy, r.Version
	if out.Enabled && connected && conn.ParentExpiresAt.Valid {
		exp := time.UnixMicro(conn.ParentExpiresAt.Int64).UTC()
		if svc.s.Now().Add(time.Duration(out.TTLSeconds) * time.Second).After(exp) {
			out.Warnings = append(out.Warnings, parentExpiryWarning(exp, spaceID, out.TTLSeconds))
		}
	}
	if out.Enabled && !connected {
		out.Warnings = append(out.Warnings, "No Harbor Pilot connection is configured: launches of the listed harnesses are refused.")
	}
	return out, nil
}

func (in *SpaceInput) normalize() error {
	v := &apperr.Validation{}
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	if in.ProjectID != "" && !reProjectID.MatchString(in.ProjectID) {
		v.Add("project_id", "must be a Harbor Pilot project id (letters, digits, . _ : -, at most 64)")
	}
	if in.Enabled && in.ProjectID == "" {
		v.Add("project_id", "is required when Harbor Pilot is enabled")
	}
	if len(in.Harnesses) == 0 {
		in.Harnesses = slices.Clone(DefaultHarnesses)
	}
	for _, h := range in.Harnesses {
		if !reHarness.MatchString(h) {
			v.Add("harnesses", "%q is not a harness kind (lowercase letters, digits and dashes)", h)
		}
	}
	slices.Sort(in.Harnesses)
	in.Harnesses = slices.Compact(in.Harnesses)
	switch {
	case in.TTLSeconds == 0:
		in.TTLSeconds = DefaultTTLSeconds
	case in.TTLSeconds < MinTTLSeconds || in.TTLSeconds > MaxTTLSeconds:
		v.Add("ttl_seconds", "must be between %d (15 minutes) and %d (7 days)", MinTTLSeconds, MaxTTLSeconds)
	}
	switch in.OnBudgetStatusUnavailable {
	case "":
		in.OnBudgetStatusUnavailable = BudgetProceed
	case BudgetProceed, BudgetRefuse:
	default:
		v.Add("on_budget_status_unavailable", "must be proceed or refuse")
	}
	for _, a := range in.AllowedModelAliases {
		if !reAlias.MatchString(a) {
			v.Add("allowed_model_aliases", "%q is not a model alias", a)
		}
	}
	slices.Sort(in.AllowedModelAliases)
	in.AllowedModelAliases = slices.Compact(in.AllowedModelAliases)
	if in.AllowedModelAliases == nil {
		in.AllowedModelAliases = []string{}
	}
	return v.Err()
}

// projectProbeTTL is the lifetime of the credential a project check mints (and revokes at once).
const projectProbeTTL = 60

// checkProject validates an enabled space's project against the connection: with a service-principal token the project
// must exist and be given by id, and a probe mint bound to it must succeed (the project is the binding's or one of its
// allowed projects). It returns the version of the connection it checked against (the caller's transaction makes sure
// the connection, its binding above all, has not changed since) and a warning when the probe credential could not be
// revoked at once.
func (svc *Service) checkProject(ctx context.Context, projectID string) (int64, string, error) {
	r, err := svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", errNotConnected
	} else if err != nil {
		return 0, "", err
	}
	parent, err := svc.open(parentAAD(r.ID), r.ParentTokenEnc, "parent")
	if err != nil {
		return 0, "", err
	}
	service := ""
	if len(r.ServiceTokenEnc) > 0 {
		if service, err = svc.open(serviceAAD(r.ID), r.ServiceTokenEnc, "service-principal"); err != nil {
			return 0, "", err
		}
	}
	c, err := svc.client(r.BaseUrl, r.AllowInsecureHttp == 1)
	if err != nil {
		return 0, "", err
	}
	if service != "" {
		p, err := c.GetProject(ctx, service, projectID)
		switch {
		case IsClass(err, ClassNotFound):
			return 0, "", validationErr("project_id", "Harbor Pilot has no project %q", projectID)
		case err != nil:
			return 0, "", hpField("project_id", err)
		case p.ID != projectID:
			return 0, "", validationErr("project_id", "%q is a project name; use its id %s", projectID, p.ID)
		}
	}
	val, err := svc.probeParent(ctx, c, parent, map[string]string{"project_id": projectID}, projectProbeTTL)
	if err != nil {
		var he *Error
		if errors.As(err, &he) && he.Class == ClassInvalid && strings.Contains(he.Message, "project") {
			return 0, "", validationErr("project_id", "Harbor Pilot refused project %q for the connection's client binding: it must be the binding's project or one of its allowed projects", projectID)
		}
		return 0, "", hpField("project_id", err)
	}
	warning := ""
	if val.unrevoked != nil {
		warning = svc.unrevokedWarning(ctx, val, "project validation")
	}
	return r.Version, warning, nil
}

func validationErr(field, format string, args ...any) error {
	v := &apperr.Validation{}
	v.Add(field, format, args...)
	return v.Err()
}

// PutSpace sets a space's opt-in: created without If-Match (version 0) or replaced with it. Enabling needs the
// connection, and the project is checked against Harbor Pilot first (outside any transaction).
//
// The project decides who pays, so setting or changing it needs account:write (canSetProject, ADR-0060 decision 2,
// the operator's decision of 2026-11-07); the rest of the settings need only space_config:write. An empty project_id
// on an update keeps the stored project. The check runs before the project probe, so a caller who may not set the
// project triggers no call to Harbor Pilot, and again in the transaction against the stored row.
func (svc *Service) PutSpace(ctx context.Context, actor audit.Actor, spaceID string, version int64, in SpaceInput, canSetProject bool) (*SpaceSettings, error) {
	if _, err := svc.s.R().GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return nil, err
	}
	cur, err := svc.s.R().GetSpaceHarborPilot(ctx, spaceID)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := checkVersion(exists, cur.Version, version); err != nil {
		return nil, err
	}
	if exists && strings.TrimSpace(in.ProjectID) == "" {
		in.ProjectID = cur.ProjectID
	}
	if err := in.normalize(); err != nil {
		return nil, err
	}
	if in.ProjectID != cur.ProjectID && !canSetProject {
		return nil, errProjectNeedsAdmin
	}
	var connVersion int64
	var warning string
	if in.Enabled {
		if connVersion, warning, err = svc.checkProject(ctx, in.ProjectID); err != nil {
			return nil, err
		}
	}
	hj, _ := json.Marshal(in.Harnesses)
	aj, _ := json.Marshal(in.AllowedModelAliases)
	err = svc.s.W().TxNamed(ctx, "harborpilot.put_space", func(tx *store.Tx) error {
		now := tx.NowMicros()
		if in.Enabled {
			conn, err := tx.Q.GetHarborPilotConnection(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				return errNotConnected
			} else if err != nil {
				return err
			}
			if conn.Version != connVersion {
				return apperr.New(apperr.CodeConflict, "the Harbor Pilot connection changed while the project was checked; retry")
			}
		}
		row, err := tx.Q.GetSpaceHarborPilot(ctx, spaceID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := checkVersion(err == nil, row.Version, version); err != nil {
			return err
		}
		if in.ProjectID != row.ProjectID && !canSetProject {
			return errProjectNeedsAdmin
		}
		action := "created"
		if version == 0 {
			if err := tx.Q.InsertSpaceHarborPilot(ctx, db.InsertSpaceHarborPilotParams{SpaceID: spaceID, Enabled: b2i(in.Enabled), ProjectID: in.ProjectID,
				HarnessesJson: string(hj), TtlSeconds: in.TTLSeconds, OnBudgetStatusUnavailable: in.OnBudgetStatusUnavailable, AllowedModelAliasesJson: string(aj),
				CreatedAt: now, CreatedBy: actorRef(actor), UpdatedAt: now, UpdatedBy: actorRef(actor)}); err != nil {
				if store.IsConstraint(err) {
					return apperr.New(apperr.CodeConflict, "the space's Harbor Pilot settings were created meanwhile; reload and retry")
				}
				return err
			}
		} else {
			action = "updated"
			n, err := tx.Q.UpdateSpaceHarborPilot(ctx, db.UpdateSpaceHarborPilotParams{Enabled: b2i(in.Enabled), ProjectID: in.ProjectID, HarnessesJson: string(hj),
				TtlSeconds: in.TTLSeconds, OnBudgetStatusUnavailable: in.OnBudgetStatusUnavailable, AllowedModelAliasesJson: string(aj), Now: now,
				UpdatedBy: actorRef(actor), SpaceID: spaceID, Version: version})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodeConflict, "the space's Harbor Pilot settings changed; reload and retry")
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_harbor_pilot", spaceID, action, version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "harbor_pilot.space." + action, Target: audit.Target{Type: "space_harbor_pilot", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "enabled": in.Enabled, "project_id": in.ProjectID, "project_changed": in.ProjectID != row.ProjectID,
				"harnesses": in.Harnesses, "ttl_seconds": in.TTLSeconds,
				"on_budget_status_unavailable": in.OnBudgetStatusUnavailable, "allowed_model_aliases": in.AllowedModelAliases}})
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := svc.GetSpace(ctx, spaceID)
	if err == nil && warning != "" {
		out.Warnings = append(out.Warnings, warning)
	}
	return out, err
}

func checkVersion(exists bool, cur, sent int64) error {
	switch {
	case !exists && sent != 0:
		return apperr.New(apperr.CodeNotFound, "the space has no Harbor Pilot settings yet: create them without If-Match")
	case exists && sent == 0:
		return apperr.New(apperr.CodeConflict, "the space has Harbor Pilot settings: replace them with If-Match \"%d\"", cur)
	case exists && sent != cur:
		return apperr.New(apperr.CodeConflict, "the space's Harbor Pilot settings changed (version %d, you sent %d); reload and retry", cur, sent)
	}
	return nil
}

// DeleteSpace removes a space's settings (version-checked): the space no longer routes runs through Harbor Pilot.
func (svc *Service) DeleteSpace(ctx context.Context, actor audit.Actor, spaceID string, version int64) error {
	return svc.s.W().TxNamed(ctx, "harborpilot.delete_space", func(tx *store.Tx) error {
		cur, err := tx.Q.GetSpaceHarborPilot(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "the space has no Harbor Pilot settings")
		} else if err != nil {
			return err
		}
		n, err := tx.Q.DeleteSpaceHarborPilot(ctx, db.DeleteSpaceHarborPilotParams{SpaceID: spaceID, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the space's Harbor Pilot settings changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_harbor_pilot", spaceID, "deleted", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "harbor_pilot.space.deleted", Target: audit.Target{Type: "space_harbor_pilot", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "was_enabled": cur.Enabled == 1, "project_id": cur.ProjectID}})
		return err
	})
}

package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/skills"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Skill is a catalog skill with its versions (SM-MCP-004).
type Skill struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source" enum:"custom,builtin"`
	SourceRef   string          `json:"source_ref,omitempty" doc:"Where a built-in skill comes from"`
	Latest      int64           `json:"latest"`
	Versions    []*SkillVersion `json:"versions,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// SkillVersion is a draft or an immutable published set of files.
type SkillVersion struct {
	ID          string            `json:"id"`
	SkillID     string            `json:"skill_id"`
	Number      int64             `json:"number"`
	State       string            `json:"state" enum:"draft,published"`
	Files       map[string]string `json:"files" doc:"Relative path → text; SKILL.md is required"`
	SHA256      string            `json:"sha256"`
	Notes       string            `json:"notes,omitempty"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	PublishedBy string            `json:"published_by,omitempty"`
	Version     int64             `json:"version"`
}

// SkillRef assigns a skill to a template or space role: following its latest version, or pinned.
type SkillRef struct {
	SkillID string `json:"skill_id"`
	Pin     string `json:"pin,omitempty" doc:"A published version ID; empty follows the latest"`
}

// ResolvedSkill is a skill a launch gets.
type ResolvedSkill struct {
	Slug      string            `json:"slug"`
	VersionID string            `json:"version_id"`
	Number    int64             `json:"number"`
	Pinned    bool              `json:"pinned"`
	SHA256    string            `json:"sha256"`
	Origin    string            `json:"origin"`
	Files     map[string]string `json:"-"`
}

const (
	maxSkillFiles = 32
	maxSkillBytes = 512 << 10
)

var (
	reSkillPath  = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)
	reFrontName  = regexp.MustCompile(`(?m)^name:\s*([^\s#]+)\s*$`)
	reFrontDescr = regexp.MustCompile(`(?m)^description:\s*\S`)
)

// validateFiles checks a skill's files: text only, contained paths, SKILL.md with frontmatter naming the skill. Files
// are written read/write for the owner only and never made executable: a skill is guidance, never an install hook.
func validateFiles(slug string, files map[string]string) error {
	v := &apperr.Validation{}
	if len(files) == 0 || len(files) > maxSkillFiles {
		v.Add("files", "1 to %d files", maxSkillFiles)
	}
	total := 0
	for p, c := range files {
		total += len(p) + len(c)
		if !reSkillPath.MatchString(p) || strings.Contains(p, "..") || len(p) > 200 {
			v.Add("files", "path %q must be a relative path inside the skill", p)
		}
		if !utf8.ValidString(c) || strings.ContainsRune(c, 0) {
			v.Add("files", "%s must be UTF-8 text", p)
		}
	}
	if total > maxSkillBytes {
		v.Add("files", "at most %d bytes in total", maxSkillBytes)
	}
	md, ok := files["SKILL.md"]
	if !ok {
		v.Add("files", "SKILL.md is required")
	} else {
		front := ""
		if strings.HasPrefix(md, "---\n") {
			if end := strings.Index(md[4:], "\n---"); end >= 0 {
				front = md[4 : 4+end]
			}
		}
		m := reFrontName.FindStringSubmatch(front)
		if front == "" || m == nil || !reFrontDescr.MatchString(front) {
			v.Add("files", "SKILL.md starts with --- frontmatter --- that has name and description")
		} else if m[1] != slug {
			v.Add("files", "SKILL.md's name (%s) must be the skill's slug (%s)", m[1], slug)
		}
	}
	return v.Err()
}

func filesHash(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(files[k])
		b.WriteByte(0)
	}
	return hash(b.String())
}

func skillVersionOut(r db.SkillVersion) *SkillVersion {
	files := map[string]string{}
	_ = json.Unmarshal([]byte(r.FilesJson), &files)
	return &SkillVersion{ID: r.ID, SkillID: r.SkillID, Number: r.Number, State: r.State, Files: files, SHA256: filesHash(files), Notes: r.Notes,
		PublishedAt: ptime(r.PublishedAt), PublishedBy: r.PublishedBy, Version: r.Version}
}

// CreateSkill adds a skill with a first draft.
func (svc *Service) CreateSkill(ctx context.Context, actor audit.Actor, slug, name, description string, files map[string]string) (*Skill, error) {
	return svc.createSkill(ctx, actor, slug, name, description, "custom", "", files)
}

func (svc *Service) createSkill(ctx context.Context, actor audit.Actor, slug, name, description, source, ref string, files map[string]string) (*Skill, error) {
	v := &apperr.Validation{}
	if !reSlug.MatchString(slug) {
		v.Add("slug", "must be 1-63 lowercase letters, digits or dashes")
	}
	if strings.TrimSpace(name) == "" || len(name) > 120 || len(description) > 2000 {
		v.Add("name", "is required (at most 120 characters)")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := validateFiles(slug, files); err != nil {
		return nil, err
	}
	fj, _ := json.Marshal(files)
	id := ids.New(ids.Skill)
	err := svc.s.W().TxNamed(ctx, "catalog.skill_create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSkillBySlug(ctx, slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a skill %q exists", slug)
		}
		if err := tx.Q.InsertSkill(ctx, db.InsertSkillParams{ID: id, Slug: slug, Name: name, Description: description, Source: source, SourceRef: ref,
			CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := tx.Q.InsertSkillVersion(ctx, db.InsertSkillVersionParams{ID: ids.New(ids.SkillVersion), SkillID: id, FilesJson: string(fj), CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "skill.created", "skill", id, map[string]any{"slug": slug, "source": source})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetSkill(ctx, id)
}

// SaveSkillDraft creates (version 0) or updates the skill's draft.
func (svc *Service) SaveSkillDraft(ctx context.Context, actor audit.Actor, skillID string, version int64, files map[string]string, notes string) (*Skill, error) {
	sk, err := svc.s.R().GetSkill(ctx, skillID)
	if err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "skill not found")
	}
	if err := validateFiles(sk.Slug, files); err != nil {
		return nil, err
	}
	if len(notes) > 2000 {
		return nil, apperr.New(apperr.CodeInvalid, "notes: at most 2000 characters")
	}
	fj, _ := json.Marshal(files)
	err = svc.s.W().TxNamed(ctx, "catalog.skill_draft", func(tx *store.Tx) error {
		d, err := tx.Q.GetSkillDraft(ctx, skillID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "the draft was published meanwhile")
			}
			if err := tx.Q.InsertSkillVersion(ctx, db.InsertSkillVersionParams{ID: ids.New(ids.SkillVersion), SkillID: skillID, FilesJson: string(fj), Notes: notes,
				CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if d.Version != version {
				return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, d.Version)
			}
			if _, err := tx.Q.UpdateSkillDraft(ctx, db.UpdateSkillDraftParams{FilesJson: string(fj), Notes: notes, UpdatedAt: tx.NowMicros(), ID: d.ID, Version: d.Version}); err != nil {
				return err
			}
		}
		return svc.log(ctx, tx, actor, "skill.draft_saved", "skill", skillID, nil)
	})
	if err != nil {
		return nil, err
	}
	return svc.GetSkill(ctx, skillID)
}

// PublishSkill publishes the draft as the next immutable version.
func (svc *Service) PublishSkill(ctx context.Context, actor audit.Actor, skillID string, version int64) (*Skill, error) {
	err := svc.s.W().TxNamed(ctx, "catalog.skill_publish", func(tx *store.Tx) error {
		d, err := tx.Q.GetSkillDraft(ctx, skillID)
		if err != nil {
			return apperr.New(apperr.CodePrecondition, "the skill has no draft to publish")
		}
		if d.Version != version {
			return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, d.Version)
		}
		next := int64(1)
		if l, err := tx.Q.LatestSkillVersion(ctx, skillID); err == nil {
			next = l.Number + 1
		}
		if _, err := tx.Q.PublishSkillVersion(ctx, db.PublishSkillVersionParams{Number: next, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, By: actor.ID,
			ID: d.ID, Version: d.Version}); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "skill.published", "skill", skillID, map[string]any{"number": next})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetSkill(ctx, skillID)
}

// GetSkill returns a skill (by ID or slug) with its versions.
func (svc *Service) GetSkill(ctx context.Context, id string) (*Skill, error) {
	q := svc.s.R()
	r, err := q.GetSkill(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		r, err = q.GetSkillBySlug(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "skill not found")
	} else if err != nil {
		return nil, err
	}
	out := &Skill{ID: r.ID, Slug: r.Slug, Name: r.Name, Description: r.Description, Source: r.Source, SourceRef: r.SourceRef,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), Versions: []*SkillVersion{}}
	vs, err := q.ListSkillVersions(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		out.Versions = append(out.Versions, skillVersionOut(v))
		if v.State == "published" && v.Number > out.Latest {
			out.Latest = v.Number
		}
	}
	return out, nil
}

// ListSkills returns every skill (versions without file bodies are not trimmed: skills are small).
func (svc *Service) ListSkills(ctx context.Context) ([]*Skill, error) {
	rows, err := svc.s.R().ListSkills(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Skill, 0, len(rows))
	for _, r := range rows {
		s, err := svc.GetSkill(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// SeedBuiltins adds the built-in skills (published as version 1) when they are missing.
func (svc *Service) SeedBuiltins(ctx context.Context) error {
	for _, b := range skills.Builtins() {
		if _, err := svc.s.R().GetSkillBySlug(ctx, b.Slug); err == nil {
			continue
		}
		sk, err := svc.createSkill(ctx, audit.System, b.Slug, b.Name, b.Description, "builtin", b.SourceRef, b.Files)
		if err != nil {
			return err
		}
		if _, err := svc.PublishSkill(ctx, audit.System, sk.ID, sk.Versions[0].Version); err != nil {
			return err
		}
	}
	return nil
}

func parseSkillRefs(s string) []SkillRef {
	out := []SkillRef{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func (svc *Service) skillRefsJSON(ctx context.Context, v *apperr.Validation, refs []SkillRef) string {
	if len(refs) > 20 {
		v.Add("skills", "at most 20 skills")
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if seen[r.SkillID] {
			v.Add("skills", "skill %s is listed twice", r.SkillID)
		}
		seen[r.SkillID] = true
		if _, err := svc.s.R().GetSkill(ctx, r.SkillID); err != nil {
			v.Add("skills", "%s is not a skill", r.SkillID)
			continue
		}
		if r.Pin != "" {
			pv, err := svc.s.R().GetSkillVersion(ctx, r.Pin)
			if err != nil || pv.SkillID != r.SkillID || pv.State != "published" {
				v.Add("skills", "pin %s must be a published version of %s", r.Pin, r.SkillID)
			}
		}
	}
	if refs == nil {
		refs = []SkillRef{}
	}
	b, _ := json.Marshal(refs)
	return string(b)
}

// resolveSkills merges a template's and a space role's skills (the space role's pin wins) into the versions a launch
// gets; a skill without a published version is skipped and reported in the manifest.
func resolveSkills(ctx context.Context, q *db.Queries, tplRefs, roleRefs []SkillRef) ([]ResolvedSkill, []string) {
	type pick struct {
		ref    SkillRef
		origin string
	}
	order := []string{}
	picks := map[string]pick{}
	for _, r := range tplRefs {
		if _, ok := picks[r.SkillID]; !ok {
			order = append(order, r.SkillID)
		}
		picks[r.SkillID] = pick{r, OriginTemplate}
	}
	for _, r := range roleRefs {
		if _, ok := picks[r.SkillID]; !ok {
			order = append(order, r.SkillID)
		}
		picks[r.SkillID] = pick{r, OriginSpaceRole}
	}
	var out []ResolvedSkill
	var skipped []string
	for _, id := range order {
		p := picks[id]
		sk, err := q.GetSkill(ctx, id)
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		var v db.SkillVersion
		if p.ref.Pin != "" {
			v, err = q.GetSkillVersion(ctx, p.ref.Pin)
		} else {
			v, err = q.LatestSkillVersion(ctx, id)
		}
		if err != nil {
			skipped = append(skipped, sk.Slug)
			continue
		}
		sv := skillVersionOut(v)
		out = append(out, ResolvedSkill{Slug: sk.Slug, VersionID: v.ID, Number: v.Number, Pinned: p.ref.Pin != "", SHA256: sv.SHA256, Origin: p.origin, Files: sv.Files})
	}
	return out, skipped
}

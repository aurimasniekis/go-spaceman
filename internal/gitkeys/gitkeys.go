// Package gitkeys keeps shared Git SSH keys (ADR-0058, SM-REPO-005). An administrator generates a key, adds its public
// half to a Git account (preferably a read-only machine user), and lets spaces use it. A space's repositories can then
// clone and fetch with it instead of each having its own deploy key (ADR-0057). The private half is sealed with the
// keyring's credential purpose; it leaves central only on the wire message of a clone or fetch, as repository keys do.
package gitkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Key is a shared Git SSH key: only its public half and fingerprint are ever shown.
type Key struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	PublicKey   string     `json:"public_key" doc:"authorized_keys format: add it to the Git account (or as a deploy key) that may read the repositories"`
	Fingerprint string     `json:"fingerprint" doc:"SHA256 fingerprint"`
	AllSpaces   bool       `json:"all_spaces" doc:"Every space may use it; otherwise only the listed spaces"`
	Spaces      []string   `json:"spaces" doc:"Spaces that may use it (when all_spaces is false)"`
	UsedBy      []RepoRef  `json:"used_by" doc:"Repositories that clone with it"`
	RotatedAt   *time.Time `json:"rotated_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CreatedBy   string     `json:"created_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
	UpdatedBy   string     `json:"updated_by"`
	Version     int64      `json:"version"`
}

// RepoRef is a repository that uses a key.
type RepoRef struct {
	SpaceID      string `json:"space_id"`
	RepositoryID string `json:"repository_id"`
	Name         string `json:"name"`
}

// Input creates or edits a key's name, description and the spaces that may use it.
type Input struct {
	Name        string   `json:"name" maxLength:"63" doc:"Unique: lowercase letters, digits, . _ -"`
	Description string   `json:"description,omitempty" maxLength:"500"`
	AllSpaces   bool     `json:"all_spaces,omitempty" doc:"Let every space use the key"`
	Spaces      []string `json:"spaces,omitempty" maxItems:"500" doc:"Space IDs that may use the key (ignored with all_spaces)"`
}

// Service manages shared Git SSH keys.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	kr      *keyring.Keyring
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, kr *keyring.Keyring) *Service {
	return &Service{s: s, audit: aw, changes: ch, kr: kr}
}

var (
	reName      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	errNotFound = apperr.New(apperr.CodeNotFound, "Git SSH key not found")
)

func aad(id string) []byte { return []byte("git_ssh_key|" + id) }

func actorRef(a audit.Actor) string {
	if a.Type == audit.ActorRoot {
		return "root"
	}
	return a.Type + ":" + a.ID
}

// NewKeyPair generates an Ed25519 key: the private key in OpenSSH PEM form, the authorized_keys line (with comment)
// and its fingerprint.
func NewKeyPair(comment string) (privatePEM []byte, authorized, fingerprint string, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, "", "", err
	}
	authorized = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment
	return pem.EncodeToMemory(block), authorized, ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// Fingerprint is the SHA256 fingerprint of an authorized_keys line ("" when it does not parse).
func Fingerprint(authorized string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

func (in *Input) validate(ctx context.Context, q *db.Queries) error {
	v := &apperr.Validation{}
	in.Name = strings.TrimSpace(in.Name)
	if !reName.MatchString(in.Name) {
		v.Add("name", "must be 1-63 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	if len(in.Description) > 500 {
		v.Add("description", "is longer than 500 characters")
	}
	if in.AllSpaces {
		in.Spaces = nil
	}
	slices.Sort(in.Spaces)
	in.Spaces = slices.Compact(in.Spaces)
	for _, id := range in.Spaces {
		if _, err := q.GetSpace(ctx, id); err != nil {
			v.Add("spaces", "space %q does not exist", id)
		}
	}
	return v.Err()
}

func (svc *Service) setSpaces(ctx context.Context, tx *store.Tx, id string, spaces []string) error {
	if err := tx.Q.DeleteGitKeySpaces(ctx, id); err != nil {
		return err
	}
	for _, sp := range spaces {
		if err := tx.Q.InsertGitKeySpace(ctx, db.InsertGitKeySpaceParams{KeyID: id, SpaceID: sp}); err != nil {
			return err
		}
	}
	return nil
}

// Create generates a key and stores it sealed.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, in Input) (*Key, error) {
	if svc.kr == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "Git SSH keys need the keyring")
	}
	if err := in.validate(ctx, svc.s.R()); err != nil {
		return nil, err
	}
	id := ids.New(ids.GitKey)
	priv, public, fp, err := NewKeyPair("spaceman-" + in.Name)
	if err != nil {
		return nil, err
	}
	sealed, err := svc.kr.Seal(keyring.PurposeCredential, aad(id), priv)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "gitkeys.create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetGitKeyByName(ctx, in.Name); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a Git SSH key named %q exists", in.Name)
		}
		if err := tx.Q.InsertGitKey(ctx, db.InsertGitKeyParams{ID: id, Name: in.Name, Description: in.Description, PublicKey: public, PrivateEnc: sealed,
			AllSpaces: b2i(in.AllSpaces), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros(), UpdatedBy: actorRef(actor)}); err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeAlreadyExists, "a Git SSH key named %q exists", in.Name)
			}
			return err
		}
		if err := svc.setSpaces(ctx, tx, id, in.Spaces); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "git_ssh_key", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "git_ssh_key.created", Target: audit.Target{Type: "git_ssh_key", ID: id},
			Details: map[string]any{"name": in.Name, "fingerprint": fp, "all_spaces": in.AllSpaces, "spaces": in.Spaces}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// Get returns a key with the spaces that may use it and the repositories that do.
func (svc *Service) Get(ctx context.Context, id string) (*Key, error) {
	r, err := svc.s.R().GetGitKey(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	return svc.out(ctx, r)
}

// List returns every key.
func (svc *Service) List(ctx context.Context) ([]*Key, error) {
	rows, err := svc.s.R().ListGitKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Key, 0, len(rows))
	for _, r := range rows {
		k, err := svc.out(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

func (svc *Service) out(ctx context.Context, r db.GitSshKey) (*Key, error) {
	q := svc.s.R()
	spaces, err := q.ListGitKeySpaces(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	used, err := q.ListRepositoriesUsingGitKey(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	k := &Key{ID: r.ID, Name: r.Name, Description: r.Description, PublicKey: r.PublicKey, Fingerprint: Fingerprint(r.PublicKey), AllSpaces: r.AllSpaces == 1,
		Spaces: spaces, UsedBy: make([]RepoRef, 0, len(used)), CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy,
		UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), UpdatedBy: r.UpdatedBy, Version: r.Version}
	if k.Spaces == nil {
		k.Spaces = []string{}
	}
	if r.RotatedAt.Valid {
		t := time.UnixMicro(r.RotatedAt.Int64).UTC()
		k.RotatedAt = &t
	}
	for _, u := range used {
		k.UsedBy = append(k.UsedBy, RepoRef{SpaceID: u.SpaceID, RepositoryID: u.ID, Name: u.Name})
	}
	return k, nil
}

// Update edits a key's name, description and spaces (version-checked). A space taken off the list while one of its
// repositories uses the key is refused: change those repositories first.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, id string, version int64, in Input) (*Key, error) {
	if err := in.validate(ctx, svc.s.R()); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "gitkeys.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetGitKey(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if other, err := tx.Q.GetGitKeyByName(ctx, in.Name); err == nil && other.ID != id {
			return apperr.New(apperr.CodeAlreadyExists, "a Git SSH key named %q exists", in.Name)
		}
		if !in.AllSpaces {
			used, err := tx.Q.ListRepositoriesUsingGitKey(ctx, id)
			if err != nil {
				return err
			}
			for _, u := range used {
				if !slices.Contains(in.Spaces, u.SpaceID) {
					return apperr.New(apperr.CodePrecondition, "repository %s in space %s uses this key; choose another key for it before taking the space off the list", u.Name, u.SpaceID)
				}
			}
		}
		n, err := tx.Q.UpdateGitKey(ctx, db.UpdateGitKeyParams{Name: in.Name, Description: in.Description, AllSpaces: b2i(in.AllSpaces), Now: tx.NowMicros(),
			UpdatedBy: actorRef(actor), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the key changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		if err := svc.setSpaces(ctx, tx, id, in.Spaces); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "git_ssh_key", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "git_ssh_key.updated", Target: audit.Target{Type: "git_ssh_key", ID: id},
			Details: map[string]any{"name": in.Name, "all_spaces": in.AllSpaces, "spaces": in.Spaces}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// Rotate replaces the key pair at once (version-checked); the new public key must be added where the old one was.
func (svc *Service) Rotate(ctx context.Context, actor audit.Actor, id string, version int64) (*Key, error) {
	if svc.kr == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "Git SSH keys need the keyring")
	}
	err := svc.s.W().TxNamed(ctx, "gitkeys.rotate", func(tx *store.Tx) error {
		cur, err := tx.Q.GetGitKey(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if version == 0 {
			version = cur.Version
		}
		priv, public, fp, err := NewKeyPair("spaceman-" + cur.Name)
		if err != nil {
			return err
		}
		sealed, err := svc.kr.Seal(keyring.PurposeCredential, aad(id), priv)
		if err != nil {
			return err
		}
		n, err := tx.Q.RotateGitKey(ctx, db.RotateGitKeyParams{PublicKey: public, PrivateEnc: sealed, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			UpdatedBy: actorRef(actor), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the key changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		if err := svc.changes.Record(ctx, tx, "", "git_ssh_key", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "git_ssh_key.rotated", Target: audit.Target{Type: "git_ssh_key", ID: id},
			Details: map[string]any{"name": cur.Name, "fingerprint": fp, "previous_fingerprint": Fingerprint(cur.PublicKey)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// Delete removes a key that no repository uses.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "gitkeys.delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetGitKey(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		used, err := tx.Q.ListRepositoriesUsingGitKey(ctx, id)
		if err != nil {
			return err
		}
		if len(used) > 0 {
			return apperr.New(apperr.CodeConflict, "%d repositories use this key (first: %s in %s); choose another key for them first", len(used), used[0].Name, used[0].SpaceID)
		}
		if _, err := tx.Q.DeleteGitKey(ctx, id); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "git_ssh_key", id, "deleted", cur.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "git_ssh_key.deleted", Target: audit.Target{Type: "git_ssh_key", ID: id},
			Details: map[string]any{"name": cur.Name, "fingerprint": Fingerprint(cur.PublicKey)}})
		return err
	})
}

// Allowed reports whether a space may use a key (within the caller's transaction when q is a transaction's).
func Allowed(ctx context.Context, q *db.Queries, spaceID, keyID string) (bool, error) {
	n, err := q.GitKeyAllowed(ctx, db.GitKeyAllowedParams{ID: keyID, SpaceID: spaceID})
	return n > 0, err
}

// Private returns a key's private half (OpenSSH PEM) for a space that may use it. Only the node hub calls it, through
// the repository service, to put the key on the wire message of a clone or fetch.
func (svc *Service) Private(ctx context.Context, spaceID, keyID string) (string, error) {
	ok, err := Allowed(ctx, svc.s.R(), spaceID, keyID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil // no longer allowed for the space: the node's own authentication applies, and the clone says why
	}
	r, err := svc.s.R().GetGitKey(ctx, keyID)
	if err != nil {
		return "", err
	}
	if svc.kr == nil {
		return "", errors.New("shared Git SSH keys need the keyring")
	}
	pt, err := svc.kr.Open(keyring.PurposeCredential, aad(keyID), r.PrivateEnc)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ForSpace lists the keys a space may use (what its repositories can choose from).
func (svc *Service) ForSpace(ctx context.Context, spaceID string) ([]*Key, error) {
	all, err := svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Key, 0, len(all))
	for _, k := range all {
		if k.AllSpaces || slices.Contains(k.Spaces, spaceID) {
			c := *k
			c.Spaces, c.UsedBy = []string{}, []RepoRef{} // a space sees the key, not who else may use it
			out = append(out, &c)
		}
	}
	return out, nil
}

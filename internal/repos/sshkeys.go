package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Repository SSH keys (ADR-0057, SM-REPO-005): SpaceMan generates an Ed25519 key per repository, keeps the private half
// sealed with the keyring's credential purpose and shows only the public half. The node gets the key only on the wire
// message of a clone or fetch (Key), serves it from an in-memory agent for that operation and never stores it.

// Known hosts bounds.
const (
	maxKnownHostsLines = 32
	maxKnownHostsBytes = 16 << 10
)

func sshKeyAAD(id string) []byte { return []byte("space_repository|ssh_key|" + id) }

// fingerprint is the SHA256 fingerprint of an authorized_keys line ("" when it does not parse).
func fingerprint(authorized string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// ValidateKnownHosts checks known_hosts text: at most 32 lines and 16 KiB, every line that is not empty or a comment a
// valid known_hosts entry (markers such as @revoked included).
func ValidateKnownHosts(text string) error {
	if text == "" {
		return nil
	}
	if len(text) > maxKnownHostsBytes {
		return fmt.Errorf("is longer than %d bytes", maxKnownHostsBytes)
	}
	lines := 0
	for i, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		lines++
		if lines > maxKnownHostsLines {
			return fmt.Errorf("has more than %d entries", maxKnownHostsLines)
		}
		if _, _, _, _, _, err := ssh.ParseKnownHosts([]byte(ln)); err != nil {
			return fmt.Errorf("line %d is not a known_hosts entry (host key-type base64-key)", i+1)
		}
	}
	return nil
}

// GenerateSSHKey creates the repository's SSH key, or replaces it (rotation: the old key stops being used at once; remove
// it from the Git host). version is the repository's version (If-Match).
func (svc *Service) GenerateSSHKey(ctx context.Context, actor audit.Actor, spaceID, id string, version int64) (*Repository, error) {
	if svc.o.Keyring == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "repository SSH keys need the keyring")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	var rotated bool
	err = svc.s.W().TxNamed(ctx, "repos.ssh_key", func(tx *store.Tx) error {
		cur, err := svc.current(ctx, tx, spaceID, id, version)
		if err != nil {
			return err
		}
		if cur.Url != "" && !proto.IsSSHURL(cur.Url) {
			return apperr.New(apperr.CodePrecondition, "an SSH key needs an SSH URL (git@host:owner/repo.git or ssh://); change the URL first")
		}
		rotated = len(cur.SshKeyEnc) > 0
		block, err := ssh.MarshalPrivateKey(priv, "spaceman "+cur.ID)
		if err != nil {
			return err
		}
		sealed, err := svc.o.Keyring.Seal(keyring.PurposeCredential, sshKeyAAD(cur.ID), pem.EncodeToMemory(block))
		if err != nil {
			return err
		}
		authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " spaceman-" + cur.Name
		return svc.storeKey(ctx, tx, actor, cur, sealed, authorized, rotated, ssh.FingerprintSHA256(signer.PublicKey()))
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// RemoveSSHKey deletes the repository's SSH key; the node's own Git authentication applies again.
func (svc *Service) RemoveSSHKey(ctx context.Context, actor audit.Actor, spaceID, id string, version int64) (*Repository, error) {
	err := svc.s.W().TxNamed(ctx, "repos.ssh_key_remove", func(tx *store.Tx) error {
		cur, err := svc.current(ctx, tx, spaceID, id, version)
		if err != nil {
			return err
		}
		if len(cur.SshKeyEnc) == 0 {
			return apperr.New(apperr.CodePrecondition, "the repository has no SSH key")
		}
		return svc.storeKey(ctx, tx, actor, cur, nil, "", false, "")
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

func (svc *Service) current(ctx context.Context, tx *store.Tx, spaceID, id string, version int64) (db.SpaceRepository, error) {
	cur, err := tx.Q.GetSpaceRepository(ctx, db.GetSpaceRepositoryParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, sql.ErrNoRows) {
		return cur, errNotFound
	} else if err != nil {
		return cur, err
	}
	if err := spaceCheck(ctx, tx.Q, spaceID); err != nil {
		return cur, err
	}
	if version != 0 && cur.Version != version {
		return cur, apperr.New(apperr.CodePrecondition, "the repository changed (version %d, you sent %d); reload and retry", cur.Version, version)
	}
	return cur, nil
}

func (svc *Service) storeKey(ctx context.Context, tx *store.Tx, actor audit.Actor, cur db.SpaceRepository, sealed []byte, public string, rotated bool, fp string) error {
	created := sql.NullInt64{}
	if sealed != nil {
		created = sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
	}
	n, err := tx.Q.SetRepositorySSHKey(ctx, db.SetRepositorySSHKeyParams{SshKeyEnc: sealed, SshPublicKey: public, CreatedAt: created, Now: tx.NowMicros(),
		UpdatedBy: actorRef(actor), ID: cur.ID, SpaceID: cur.SpaceID, Version: cur.Version})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.New(apperr.CodePrecondition, "the repository changed concurrently; reload and retry")
	}
	if err := svc.changes.Record(ctx, tx, cur.SpaceID, "space_repository", cur.ID, "updated", cur.Version+1); err != nil {
		return err
	}
	action, details := "space_repository.ssh_key_removed", map[string]any{"space_id": cur.SpaceID, "name": cur.Name}
	if sealed != nil {
		action, details["fingerprint"], details["rotated"] = "space_repository.ssh_key_generated", fp, rotated
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "space_repository", ID: cur.ID}, Details: details})
	return err
}

// Key returns a repository's private SSH key (OpenSSH PEM), or "" when it has none. Only the node hub calls it, to put
// the key on the wire message of a clone or fetch; it is never logged, stored elsewhere or returned by the API.
func (svc *Service) Key(ctx context.Context, spaceID, id string) (string, error) {
	r, err := svc.s.R().GetRepositorySSHKey(ctx, db.GetRepositorySSHKeyParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(r.SshKeyEnc) == 0) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if svc.o.Keyring == nil {
		return "", errors.New("repository SSH keys need the keyring")
	}
	pt, err := svc.o.Keyring.Open(keyring.PurposeCredential, sshKeyAAD(id), r.SshKeyEnc)
	if err != nil {
		return "", fmt.Errorf("open repository key: %w", err)
	}
	return string(pt), nil
}

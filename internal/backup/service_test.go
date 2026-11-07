package backup_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

func (f *fixture) service(t backup.Target, keep int) *backup.Service {
	return backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: t, Sources: f.sources(), KeepLast: keep, Interval: time.Hour,
		Config: func() []byte { return []byte("mode: development\n") }})
}

func item(t *testing.T, st *backup.Status, id string) backup.Item {
	t.Helper()
	for _, it := range st.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("backup %s not listed: %+v", id, st.Items)
	return backup.Item{}
}

// SM-STORE-005: manual backups through the service, verification, the listing (including a CLI-made backup) and
// retention by count.
func TestServiceLocal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	svc := f.service(backup.LocalTarget{Dir: dir}, 2)
	must(t, svc.Recover(f.ctx))
	first, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	if first.State != backup.StateRunning {
		t.Fatalf("start: %+v", first)
	}
	if _, err := svc.Start(f.ctx, admin, backup.TriggerManual); err != nil && !errors.Is(err, backup.ErrRunning) {
		t.Fatalf("concurrent start: %v", err)
	}
	svc.Wait()
	st, err := svc.Status(f.ctx)
	must(t, err)
	it := item(t, st, first.ID)
	if it.State != backup.StateSucceeded || !it.Stored || it.Files == 0 || it.StoredBytes == 0 || it.CreatedAt == nil || it.KEKID != f.kek.ID() ||
		st.Target != "local" || st.Location != dir || len(st.Included) == 0 || len(st.Excluded) == 0 {
		t.Fatalf("status: %+v item %+v", st, it)
	}
	if _, err := svc.StartVerify(f.ctx, admin, first.ID); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	st, _ = svc.Status(f.ctx)
	if it := item(t, st, first.ID); it.VerifyState != "ok" || it.VerifiedAt == nil {
		t.Fatalf("verify: %+v", it)
	}
	if _, err := svc.StartVerify(f.ctx, admin, "bkp_00000000000000000000000000"); err == nil {
		t.Fatal("verified a backup that does not exist")
	}

	// A backup made outside the service (the CLI) is listed as such.
	cliID := "bkp_" + strings.Repeat("0", 25) + "9"
	cli, err := backup.Create(f.ctx, f.source(), f.kek, filepath.Join(dir, cliID), backup.Options{ID: cliID, Sources: f.sources()})
	must(t, err)
	st, _ = svc.Status(f.ctx)
	if it := item(t, st, cli.Manifest.ID); it.Trigger != backup.TriggerCLI || !it.Stored {
		t.Fatalf("cli backup: %+v", it)
	}

	// Retention keeps the newest two.
	second, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	svc.Wait()
	entries, err := backup.LocalTarget{Dir: dir}.List(f.ctx)
	must(t, err)
	if len(entries) != 2 || entries[0].ID != second.ID {
		t.Fatalf("after retention: %+v", entries)
	}
	st, _ = svc.Status(f.ctx)
	for _, it := range st.Items {
		if it.ID == first.ID {
			t.Fatalf("pruned backup still listed: %+v", it)
		}
	}
}

// SM-STORE-002/005 (simulated): backups to S3-compatible storage through the fake bucket; verify and restore from it.
func TestServiceS3(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "sm-backups", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	must(t, err)
	tgt := backup.S3Target{Objects: cl, Bucket: "sm-backups", Prefix: "spaceman/", Staging: filepath.Join(f.st.Dir(), "backups", ".staging")}
	svc := f.service(tgt, 1)
	b1, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	svc.Wait()
	st, err := svc.Status(f.ctx)
	must(t, err)
	if it := item(t, st, b1.ID); it.State != backup.StateSucceeded || !it.Stored || st.Location != "s3://sm-backups/spaceman/" {
		t.Fatalf("s3 status: %+v %+v", st, it)
	}
	for _, k := range srv.Keys("sm-backups") {
		if b, _ := srv.Object("sm-backups", k); strings.Contains(string(b), f.secret) {
			t.Fatalf("object %s holds plaintext", k)
		}
	}
	if _, err := svc.StartVerify(f.ctx, admin, b1.ID); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	st, _ = svc.Status(f.ctx)
	if it := item(t, st, b1.ID); it.VerifyState != "ok" {
		t.Fatalf("s3 verify: %+v", it)
	}
	b2, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	svc.Wait()
	entries, err := tgt.List(f.ctx)
	must(t, err)
	if len(entries) != 1 || entries[0].ID != b2.ID {
		t.Fatalf("s3 retention: %+v", entries)
	}
	dir, cleanup, err := tgt.Fetch(f.ctx, b2.ID, t.TempDir())
	must(t, err)
	defer cleanup()
	must(t, f.st.Close())
	if _, err := backup.Restore(f.ctx, dir, backup.RestoreOptions{DataDir: filepath.Join(t.TempDir(), "r"), KEK: f.kek, Git: f.git}); err != nil {
		t.Fatal(err)
	}
	if err := tgt.Delete(f.ctx, "../x"); err == nil {
		t.Fatal("deleted an invalid ID")
	}
}

// A failing target records a failed run; the scheduler makes the first backup after FirstDelay with one timer.
func TestServiceScheduleAndFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources(), Schedule: true,
		Interval: time.Hour, KeepLast: 3, MaxAge: 2 * time.Hour, FirstDelay: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := svc.Status(f.ctx)
		must(t, err)
		if len(st.Items) == 1 && st.Items[0].State == backup.StateSucceeded {
			if st.Items[0].Trigger != backup.TriggerScheduled || st.NextAt == nil || !st.Schedule || st.MaxAge == "" {
				t.Fatalf("scheduled: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scheduled backup: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	svc.Wait()

	bad := backup.NewService(f.st, newKEK(t), f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources()})
	r, err := bad.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	bad.Wait()
	st, _ := bad.Status(f.ctx)
	if it := item(t, st, r.ID); it.State != backup.StateFailed || !strings.Contains(it.Error, "KEK") {
		t.Fatalf("failed run: %+v", it)
	}
}

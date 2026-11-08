# Upgrade and rollback

Upgrading SpaceMan means swapping the binary or the image. Central migrates its database at startup.

::: warning Status
SpaceMan has the pieces an upgrade relies on: forward-only migrations, the automatic pre-migration snapshot, refusal of
a newer schema, sealed backups with an offline restore, and `/readyz`. No version has been released yet, though, so an
upgrade from one release to the next has never been exercised. Running central and nodes on different versions has not
been tested either.
:::

## Rules

- **Migrations are forward-only.** There are no down migrations.
- **A pre-migration snapshot is automatic.** Before it applies migrations to an existing database, central writes a
  consistent `VACUUM INTO` copy to `<data_dir>/backups/pre-migrate-<from>-<to>-<UTC>.db` and keeps the newest three.
  If a migration fails, the database stays at the last successful version, the process exits and the error names the
  snapshot directory.
- **Never run an older binary on a newer schema.** It refuses to start with `database schema is newer than this binary`.
  Do not work around this.
- **The KEK does not change during an upgrade.** Keep it available. Snapshots and backups are useless without it.
- **Upgrade central first, then the nodes.**

## Before upgrading

1. Read the release notes and the `CHANGELOG.md` entry, especially **Security** and any breaking changes.
2. Verify the new archive with `sha256sum --check SHA256SUMS` ([installation](/operators/install#release-archives)).
3. Take a sealed backup and verify it ([Backups and restore](/operators/backup)):
   ```sh
   sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml backup create
   sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml backup verify bkp_…
   ```
4. Confirm that you have an offline copy of the KEK whose ID matches the one in the backup manifest.

## Upgrade central (systemd)

```sh
sudo systemctl stop spaceman                       # SIGTERM; graceful shutdown up to shutdown.timeout
sudo install -m 0755 -o root -g root spaceman_NEW_linux_amd64/spaceman /usr/local/bin/spaceman.new
sudo mv /usr/local/bin/spaceman /usr/local/bin/spaceman.prev   # keep the previous binary for rollback
sudo mv /usr/local/bin/spaceman.new /usr/local/bin/spaceman
sudo install -m 0644 spaceman_NEW_linux_amd64/systemd/spaceman.service /etc/systemd/system/   # if it changed
sudo systemctl daemon-reload
sudo systemctl start spaceman
journalctl -u spaceman -o cat | grep -E 'migration|snapshot|serving|error'
curl -fsS http://127.0.0.1:9090/readyz
```

**Shutdown budget.** On SIGTERM central stops in three steps, one after the other: requests and background work get
up to `shutdown.timeout` (30 s by default), background gateway token mints get 5 s, and the database gets 8 s for its
final checkpoint and for connections still in use. That is at most 43 s, inside the unit's `TimeoutStopSec=45s` (and the
compose file's `stop_grace_period: 45s`). If a database connection is still in use after its 8 s, central releases the
data-directory lock anyway and logs `closing central failed` with the number of connections; the next start is not
affected. If you raise `shutdown.timeout`, raise `TimeoutStopSec` by the same amount.

While central is down, nodes keep their runs going for the lease plus `nodes.disconnect_grace`, then apply each space's
disconnect policy ([Nodes and leases](/operators/nodes)). Keep the downtime shorter than that, or pause spaces first.

## Upgrade a node (systemd)

Restarting `spaceman-agent.service` ends every run on the node (see
[the agent unit](/operators/install#agent-sandbox)). Let runs finish, or pause the node's
spaces, then:

```sh
sudo systemctl stop spaceman-agent
sudo install -m 0755 -o root -g root spaceman_NEW_linux_amd64/spaceman-agent /usr/local/bin/spaceman-agent
sudo install -m 0644 spaceman_NEW_linux_amd64/systemd/spaceman-agent.service /etc/systemd/system/   # if it changed
sudo systemctl daemon-reload
sudo systemctl start spaceman-agent
```

Upgrade herdr-core and the harnesses separately, on your own schedule. The node reports their versions to central in
its capability inventory.

## Upgrade with containers

```sh
docker compose stop spaceman                       # stop_grace_period 45 s covers the shutdown budget below
SM_VERSION=NEW docker compose up -d
docker compose logs -f spaceman
```

Tag images by version (or reference them by digest), never by `latest`, so that a restart cannot upgrade implicitly.

## Rollback

A rollback restores state from before the upgrade. Anything written after that point is lost: runs, inbox items,
messages, approvals, audit entries, users, tokens and node enrollments.

- **Preferred: restore the backup from "Before upgrading"** with the previous binary:
  `sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml backup restore --force bkp_…` ([Backups and restore](/operators/backup)). The restore comes up **paused,
  with new execution authority**. Every space is held and every lease generation is advanced, so no node keeps running
  on authority from the newer installation. Resume each space by hand.
- **Only when the new version never served:** for example, when a migration failed at startup. In that case you can
  put the pre-migration snapshot back with the service stopped:
  1. Move `spaceman.db`, `spaceman.db-wal` and `spaceman.db-shm` aside **together**. A stale WAL must never be applied
     to a different database file.
  2. Copy the snapshot to `<data_dir>/spaceman.db`, owned by the service user, mode `0600`.
  3. Reinstall `/usr/local/bin/spaceman.prev`.

  Do not do this after the new version has served nodes. Unlike a backup restore, a snapshot does not advance lease
  generations.

Keep the moved-aside newer data until you are sure you no longer need it, then delete it.

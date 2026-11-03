# Artifact storage

Central keeps three kinds of artifacts outside its database: **attachment content**, its **copies of terminal
recordings** and **run transcripts**. They live in the artifact store, which is either directories on central's disk
(the default) or an S3-compatible bucket. The database records which attachments, recordings and transcripts exist,
who may read them and how much space they use; the store holds the bytes (ADR-0048).

| Artifact | Key in the store | Local default |
|---|---|---|
| Attachment content, addressed by its SHA-256 | `attachments/sha256/<aa>/<sha256>` | `<data_dir>/artifacts/sha256/<aa>/<sha256>` (or `artifacts.local_dir`) |
| One segment of a run's recording | `recordings/<run>/seg-NNNNNN.smrec.zst` | `<data_dir>/recordings/<run>/…` |
| A run's native transcript | `transcripts/<run>.jsonl` | `<data_dir>/transcripts/<run>.jsonl` |

Identical attachment content is stored once. Every space's attachment quota (`artifacts.space_quota_bytes`, or the
space's storage limit) counts its attachments in the database, whatever the backend holds.

## The local disk (default)

```yaml
artifacts:
  backend: local
  local_dir: /srv/spaceman-artifacts   # attachments; default <data_dir>/artifacts
```

Recordings and transcripts stay in the data directory. Uploads in progress are spooled in `<local_dir>/tmp`.

## S3-compatible storage

```yaml
artifacts:
  backend: s3
  s3:
    endpoint: https://s3.eu-central-1.amazonaws.com
    region: eu-central-1
    bucket: example-spaceman
    prefix: prod/artifacts/               # optional; ends with a slash; one installation per prefix
    access_key_file: /etc/spaceman/s3-access-key
    secret_key_file: /etc/spaceman/s3-secret-key
    use_path_style: false                 # true for MinIO
    allow_private_network: false          # true for a self-hosted endpoint on a private address
    ca_file: ""                           # extra trust roots (PEM); verification is never disabled
```

The keys are read from their files at start and never logged; the configuration holds only their paths. TLS is
required (plain HTTP only in development mode), the client follows no redirects and uses no proxy, and private
addresses are refused unless allowed. The bucket can be the backups' bucket, but `artifacts.s3.prefix` and
`backups.s3.prefix` must not overlap (central refuses the configuration).

How the S3 store works:

- **Uploads** are streamed to a spool file on central's disk while they are hashed and size-checked, then uploaded in
  one request whose SHA-256 the service verifies. Nothing holds a whole file in memory. One object is at most 5 GiB.
- **Downloads** stream from the bucket. A browser's range request (resuming a download, seeking in a file) becomes a
  range request to the bucket. A full download of an attachment or a transcript is checked against its SHA-256: when
  the stored content is damaged, the download ends short with an error instead of completing.
- **Recordings** are written to `<data_dir>/recordings/<run>/` while the node ships them (S3 has no append). When the
  recording closes or its run ends, its segments are uploaded and removed locally. Replays and downloads read both
  places, so nothing changes for viewers.
- **Transcripts** are received into a local `.part` file, checked, and uploaded.
- **Deletes** (an operator's, a retention period's, a purge's, attachment garbage collection) remove the objects.
- **When the bucket is unavailable**, attachment uploads fail (try again later). A recording or transcript that cannot
  be uploaded stays on the local disk, is served from there, and is uploaded at central's next start.

::: warning The bucket holds sensitive content
Recordings show everything a terminal displayed, and transcripts can hold secrets. SpaceMan stores them in the bucket as
it stores them on disk, without encrypting them itself. Use a bucket only SpaceMan can read, with server-side encryption
if you want it, and protect its keys like the KEK. Attachment garbage collection deletes unreferenced content under the
prefix, so never let two installations (or a restored copy) share one prefix.
:::

Central still needs local disk with the S3 store: for recordings being shipped, transcripts being received and uploads
in progress.

::: warning Verified against MinIO only
The S3 artifact store has been run against MinIO (`make test-real-s3`), not yet against AWS S3 or Cloudflare R2, and
only with path-style addressing (docs/TRACEABILITY.md, SM-STORE-002).
:::

## Moving artifacts between the disk and S3

Changing `artifacts.backend` moves nothing. Move the artifacts with `spaceman artifacts migrate` while central is
stopped (the command takes the data-directory lock). It copies what the destination lacks, checks attachment content
against its address, and can be run again after an interruption.

From the local disk to S3:

```bash
systemctl stop spaceman
sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml artifacts migrate --to s3 --dry-run   # count first
sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml artifacts migrate --to s3
# set artifacts.backend: s3 in the configuration
systemctl start spaceman
# once central serves everything from the bucket, remove the local copies (central stopped again):
sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml artifacts migrate --to s3 --remove-source
```

Back from S3 to the local disk, use `--to local` the same way. With the S3 backend, central warns at start when
attachment content is still on the local disk: it is not served until it is migrated.

## Backups

Backups hold every artifact, whichever backend is configured: with S3, the objects are copied into each backup. A
restore writes artifacts to the local folders; run `spaceman artifacts migrate --to s3` before starting a restored
installation that uses S3. See [Backups and restore](/operators/backup).

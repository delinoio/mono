---
title: Backup and Recovery
description: Logical/physical backups, PITR, WAL archiving, backup tools, and recovery strategies
tags: postgres, backup, recovery, pitr, pg_dump, pg_basebackup, wal-archiving, operations
---

# Backup and Recovery

**FUNDAMENTAL RULE: Backups are useless until you've successfully tested recovery.**

## Logical Backups (pg_dump)
Exports as SQL or custom format; portable across PG versions and architectures. Formats: `-Fp` (plain SQL), `-Fc` (custom compressed, selective restore), `-Fd` (directory, parallel with `-j`), `-Ft` (tar, avoid). Use `-Fd -j 4` for large DBs. Restore depends on the format: `-Fp` (plain SQL) is restored with `psql`, for example `psql -X -v ON_ERROR_STOP=1 -d dbname -f dump.sql` into a prepared target database; `-Fc`, `-Fd` and `-Ft` archives are restored with `pg_restore`, for example `pg_restore -d dbname file.dump` for a custom archive (directory and tar archives also use `pg_restore`). `pg_restore` options apply to archive formats only: `-j` for parallel restore and `-t tablename` for selective table restore do not work with a plain SQL dump. Slow for large DBs; RPO = backup frequency (typically 24h).

## Physical Backups (pg_basebackup)
A full `pg_basebackup` copies the entire database cluster and can be faster for large clusters. For a physical restore, use the same PostgreSQL major version as the source and a compatible platform with the same hardware architecture. [PostgreSQL standby planning](https://www.postgresql.org/docs/18/warm-standby.html#STANDBY-PLANNING)

For a move between hardware architectures, such as x86_64 to ARM64, use the [logical backup and restore procedure](#logical-backups-pg_dump). [PostgreSQL SQL dump documentation](https://www.postgresql.org/docs/18/backup-dump.html)

Flags: `-Ft -z -P` for compressed tar with progress. Manual alternative (complex; `pg_basebackup` stays the primary path): call `pg_backup_start()`, copy PGDATA **and the data of every external tablespace**, archiving symbolic links as links rather than following them, then call `pg_backup_stop()` in the same session that called `pg_backup_start()`. `pg_backup_stop()` returns three fields; save the second field byte for byte as `backup_label` and the third field byte for byte as `tablespace_map` (only if it is nonempty), both in the backup root. WAL archiving must be working so that every WAL segment from the backup start through the backup stop is archived. See [Making a Base Backup Using the Low Level API](https://www.postgresql.org/docs/current/continuous-archiving.html#BACKUP-LOWLEVEL-BASE-BACKUP).

### Verify a physical backup

On PostgreSQL 13–17, `pg_verifybackup` checks tar backups after extraction to a plain layout. On PostgreSQL 18, you can check the compressed archives directly against `backup_manifest`. Run the PostgreSQL 18 verifier on the directory containing the manifest and all the tar files. This command skips verification of the WAL required for recovery. [PostgreSQL 17 verifier](https://www.postgresql.org/docs/17/app-pgverifybackup.html) · [PostgreSQL 18 verifier](https://www.postgresql.org/docs/18/app-pgverifybackup.html)

```sh
pg_verifybackup --format=tar --no-parse-wal /path/to/tar-backup
```

For a PostgreSQL 13–17 tar backup, or to verify WAL from a PostgreSQL 18 tar backup, start with a separate, empty staging directory. [PostgreSQL base backup formats](https://www.postgresql.org/docs/18/app-pgbasebackup.html)

1. Extract `base.tar.gz` into the staging directory and copy `backup_manifest` into its root.
2. For each external tablespace, extract its `<OID>.tar.gz` into `pg_tblspc/<OID>` under the staging directory. [Backup manifest file paths](https://www.postgresql.org/docs/18/backup-manifest-files.html)
3. If the backup has a separate `pg_wal.tar.gz` or `pg_wal.tar` archive, extract it into `pg_wal` under the staging directory. If WAL is in `base.tar.gz`, it is already under `pg_wal` after extraction. If the backup lacks any WAL segments required by the manifest, retrieve them from the WAL archive and put the uncompressed segment files in `pg_wal`. [Backup manifest WAL ranges](https://www.postgresql.org/docs/18/backup-manifest-wal-ranges.html)

For an existing plain backup, check that `backup_manifest` is in the root, each `pg_tblspc/<OID>` entry leads to the backed-up tablespace files, and `pg_wal` contains the required segments. Run the verifier from the same PostgreSQL major version as the backup on the plain or staged directory. With WAL parsing enabled by default, this checks the backup against the manifest and parses the WAL required for recovery. [PostgreSQL 18 verifier](https://www.postgresql.org/docs/18/app-pgverifybackup.html)

```sh
pg_verifybackup /path/to/plain-backup
```

## PITR (Point-in-Time Recovery)
Requires base backup + continuous WAL archiving. Restores to any timestamp, transaction, or named restore point. Without PITR: restore only to backup time (potentially lose hours). With PITR: RPO = minutes. `archive_command` must return 0 ONLY when file is safely stored—premature 0 = data loss risk. `wal_level` must be `replica` or `logical` (not `minimal`).

## WAL Archiving
`archive_mode=on` plus an `archive_command` (or `archive_library`) that honors this contract. Prefer a dedicated archiver (pgBackRest, WAL-G or Barman) over a hand-written one-liner. **Do not use `test ! -f /archive/%f && cp %p /archive/%f`**: `cp` returning does not mean the data is durable, and the existence check treats every retry as a failure, even when an earlier attempt copied identical bytes, so archiving can stall on a file that is already stored. A custom archive script must follow these steps.

- **Prepare the file.** Copy `%p` to a unique temporary file in the archive directory and fsync the temporary file.
- **Publish the file.** Use an atomic operation that fails if the final name exists, such as Linux `renameat2` with `RENAME_NOREPLACE`. Use it even if an earlier check found the final name absent. If publication succeeds, fsync the archive directory before exiting 0.
- **Handle an existing file.** If the final name already exists, including when publication returns `EEXIST`, compare the published bytes with `%p` (for example, with `cmp`). If the bytes differ, exit nonzero and leave the published file untouched. If they match, fsync the published file and the archive directory before exiting 0.
- **Handle other errors.** Exit nonzero on any other publication error or on a copy, comparison, or fsync error. If the filesystem does not support atomic publication without replacement, exit nonzero. Clean up only this attempt's unused temporary file and leave the published file in place.

**Test the archive command as the postgres OS user** (not root) since permission issues are common. Monitor `pg_stat_archiver` for `failed_count`, `last_archived_time`. Archive failures prevent WAL recycling → disk fills.

## Tool Comparison
| Tool | Use case |
|------|----------|
| pg_dump | Small DBs, migrations, selective restore |
| pg_basebackup | Basic PITR, built-in |
| pgBackRest | Production—parallel, incremental, S3/GCS/Azure, retention |
| Barman | Enterprise PITR, retention policies |
| WAL-G | Cloud-native, S3/GCS/Azure |

## RPO/RTO
Logical only: RPO = backup interval (hours); RTO = hours. PITR: RPO = minutes; RTO = hours. Synchronous replication: RPO = 0; RTO = seconds to minutes (failover).

## Operational Rules
- [Verify physical backup integrity](#verify-a-physical-backup) with `pg_verifybackup` (PG 13+)
- Test recovery / PITR regularly
- Take backups from standby to avoid impacting primary
- Retention: 7 daily, 4 weekly, 12 monthly
- Monitor archive growth and backup age
- **Never assume backups work without testing**

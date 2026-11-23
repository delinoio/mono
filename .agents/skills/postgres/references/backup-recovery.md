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
Copies raw PGDATA; same major version and platform required; cross-architecture works if same endianness (e.g., x86_64 ↔ ARM64). Faster for large clusters; includes all databases. Flags: `-Ft -z -P` for compressed tar with progress. Manual alternative (complex; `pg_basebackup` stays the primary path): call `pg_backup_start()`, copy PGDATA **and the data of every external tablespace**, archiving symbolic links as links rather than following them, then call `pg_backup_stop()` in the same session that called `pg_backup_start()`. `pg_backup_stop()` returns three fields; save the second field byte for byte as `backup_label` and the third field byte for byte as `tablespace_map` (only if it is nonempty), both in the backup root. WAL archiving must be working so that every WAL segment from the backup start through the backup stop is archived. See [Making a Base Backup Using the Low Level API](https://www.postgresql.org/docs/current/continuous-archiving.html#BACKUP-LOWLEVEL-BASE-BACKUP).

## PITR (Point-in-Time Recovery)
Requires base backup + continuous WAL archiving. Restores to any timestamp, transaction, or named restore point. Without PITR: restore only to backup time (potentially lose hours). With PITR: RPO = minutes. `archive_command` must return 0 ONLY when file is safely stored—premature 0 = data loss risk. `wal_level` must be `replica` or `logical` (not `minimal`).

## WAL Archiving
`archive_mode=on` plus an `archive_command` (or `archive_library`) that honors this contract. Prefer a dedicated archiver (pgBackRest, WAL-G or Barman) over a hand-written one-liner. **Do not use `test ! -f /archive/%f && cp %p /archive/%f`**: `cp` returning does not mean the data is durable, and the existence check treats every retry as a failure, even when an earlier attempt copied identical bytes, so archiving can stall on a file that is already stored. A custom archive script must:
- **First archive** (destination absent): copy to a temporary name in the archive directory, fsync the file, rename it to the final name, fsync the directory, and only then exit 0.
- **Retry** (destination already exists): compare the bytes with `%p` (for example `cmp`) and make sure the existing file is durable (fsync it and the directory) before exiting 0.
- **Conflict** (same name, different bytes): exit nonzero and leave the existing file untouched.
- **Any failure to write or to establish durability**: exit nonzero.

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
- Verify integrity with `pg_verifybackup` (PG 13+)
- Test recovery / PITR regularly
- Take backups from standby to avoid impacting primary
- Retention: 7 daily, 4 weekly, 12 monthly
- Monitor archive growth and backup age
- **Never assume backups work without testing**

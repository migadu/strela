# Fune Backup and Restore Procedures

This document describes backup and restore procedures for Fune's SQLite database, including disaster recovery scenarios and best practices.

## Table of Contents

- [Overview](#overview)
- [Database Architecture](#database-architecture)
- [Backup Methods](#backup-methods)
- [Restore Procedures](#restore-procedures)
- [Disaster Recovery](#disaster-recovery)
- [Monitoring and Verification](#monitoring-and-verification)
- [Production Best Practices](#production-best-practices)

---

## Overview

Fune uses SQLite with Write-Ahead Logging (WAL) mode for its persistent queue. The database contains:

- **Message queue** - Pending and in-flight email messages
- **Delivery attempts** - Historical delivery attempt records
- **Callback queue** - Pending webhook notifications
- **MX cache** - Cached DNS MX records
- **Idempotency keys** - Deduplication tracking
- **IP reputation** - Source IP reputation data

**Database Location**: Configured in `config.toml` as `database_path` (default: `./queue.db`)

**Critical Components**:
- `queue.db` - Main database file
- `queue.db-wal` - Write-Ahead Log file (active transactions)
- `queue.db-shm` - Shared memory file (WAL index)

---

## Database Architecture

### WAL Mode Benefits

Fune uses SQLite's WAL (Write-Ahead Logging) mode which provides:

- **Concurrent access**: Multiple readers don't block the single writer
- **Better performance**: Write transactions don't lock the entire database
- **Crash recovery**: Uncommitted transactions can be rolled back

### WAL Mode Implications for Backups

⚠️ **Important**: When backing up a WAL-mode database, you must capture both the main database file and the WAL file to ensure consistency.

---

## Backup Methods

### Method 1: Online Backup with SQLite `.backup` Command (Recommended)

The safest method for backing up a live database is using SQLite's built-in backup API.

```bash
#!/bin/bash
# backup-fune.sh

BACKUP_DIR="/var/backups/fune"
DB_PATH="./queue.db"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/queue_backup_$TIMESTAMP.db"

# Create backup directory if it doesn't exist
mkdir -p "$BACKUP_DIR"

# Perform online backup
sqlite3 "$DB_PATH" ".backup '$BACKUP_FILE'"

# Verify backup integrity
sqlite3 "$BACKUP_FILE" "PRAGMA integrity_check;" > /dev/null 2>&1
if [ $? -eq 0 ]; then
    echo "Backup successful: $BACKUP_FILE"

    # Compress backup
    gzip "$BACKUP_FILE"
    echo "Compressed: ${BACKUP_FILE}.gz"

    # Optional: Upload to S3 or remote storage
    # aws s3 cp "${BACKUP_FILE}.gz" "s3://my-bucket/fune-backups/"
else
    echo "Backup verification failed!"
    rm -f "$BACKUP_FILE"
    exit 1
fi

# Clean up old backups (keep last 7 days)
find "$BACKUP_DIR" -name "queue_backup_*.db.gz" -mtime +7 -delete
```

**Usage**:
```bash
chmod +x backup-fune.sh
./backup-fune.sh
```

**Schedule with cron**:
```cron
# Backup every 6 hours
0 */6 * * * /path/to/backup-fune.sh >> /var/log/fune-backup.log 2>&1

# Daily backup at 2 AM
0 2 * * * /path/to/backup-fune.sh >> /var/log/fune-backup.log 2>&1
```

**Advantages**:
- ✅ Safe for online/live databases
- ✅ Atomic and consistent snapshots
- ✅ No service interruption
- ✅ Built into SQLite

### Method 2: File System Copy with WAL Checkpoint

If you need to copy the database files directly, first checkpoint the WAL to ensure all data is in the main database file.

```bash
#!/bin/bash
# backup-fune-checkpoint.sh

BACKUP_DIR="/var/backups/fune"
DB_PATH="./queue.db"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

mkdir -p "$BACKUP_DIR"

# Force WAL checkpoint to write all pending data to main DB
sqlite3 "$DB_PATH" "PRAGMA wal_checkpoint(TRUNCATE);"

# Copy database files
cp "$DB_PATH" "$BACKUP_DIR/queue_backup_$TIMESTAMP.db"
cp "${DB_PATH}-wal" "$BACKUP_DIR/queue_backup_${TIMESTAMP}-wal" 2>/dev/null || true
cp "${DB_PATH}-shm" "$BACKUP_DIR/queue_backup_${TIMESTAMP}-shm" 2>/dev/null || true

echo "Backup complete: $BACKUP_DIR/queue_backup_$TIMESTAMP.db"
```

**Advantages**:
- ✅ Simple file copy
- ✅ Can be used with filesystem snapshots

**Disadvantages**:
- ⚠️ Requires write access to database
- ⚠️ Brief pause during checkpoint

### Method 3: Filesystem Snapshots (LVM, ZFS, Btrfs)

If your filesystem supports atomic snapshots, you can use them for instant backups.

**LVM Example**:
```bash
# Create LVM snapshot
lvcreate -L 1G -s -n fune_snap /dev/vg0/fune_volume

# Mount snapshot
mkdir -p /mnt/fune_snapshot
mount /dev/vg0/fune_snap /mnt/fune_snapshot

# Copy database from snapshot
cp /mnt/fune_snapshot/queue.db /var/backups/fune/queue_$(date +%Y%m%d).db

# Unmount and remove snapshot
umount /mnt/fune_snapshot
lvremove -f /dev/vg0/fune_snap
```

**ZFS Example**:
```bash
# Create ZFS snapshot
zfs snapshot tank/fune@backup_$(date +%Y%m%d_%H%M%S)

# List snapshots
zfs list -t snapshot

# Send snapshot to backup server
zfs send tank/fune@backup_20250107 | ssh backup-server zfs receive tank/backups/fune
```

**Advantages**:
- ✅ Instant, atomic snapshots
- ✅ No impact on running service
- ✅ Space-efficient (copy-on-write)

### Method 4: Export to SQL Dump

For disaster recovery or migration, you can export the database to SQL text format.

```bash
#!/bin/bash
# export-fune.sh

DB_PATH="./queue.db"
EXPORT_FILE="fune_export_$(date +%Y%m%d_%H%M%S).sql"

# Export schema and data
sqlite3 "$DB_PATH" .dump > "$EXPORT_FILE"

# Compress
gzip "$EXPORT_FILE"

echo "Export complete: ${EXPORT_FILE}.gz"
```

**Advantages**:
- ✅ Human-readable format
- ✅ Version control friendly
- ✅ Database-agnostic (can migrate to PostgreSQL)

**Disadvantages**:
- ❌ Slower than binary backup
- ❌ Larger file size before compression

---

## Restore Procedures

### Restore from Online Backup

```bash
#!/bin/bash
# restore-fune.sh

BACKUP_FILE="/var/backups/fune/queue_backup_20250107_120000.db.gz"
DB_PATH="./queue.db"

# Stop Fune service
systemctl stop fune-server

# Backup current database (safety)
if [ -f "$DB_PATH" ]; then
    mv "$DB_PATH" "${DB_PATH}.pre-restore.$(date +%Y%m%d_%H%M%S)"
    rm -f "${DB_PATH}-wal" "${DB_PATH}-shm"
fi

# Restore backup
gunzip -c "$BACKUP_FILE" > "$DB_PATH"

# Verify integrity
sqlite3 "$DB_PATH" "PRAGMA integrity_check;"
if [ $? -eq 0 ]; then
    echo "Database restored successfully"

    # Set correct permissions
    chown fune:fune "$DB_PATH"
    chmod 640 "$DB_PATH"

    # Start Fune service
    systemctl start fune-server
else
    echo "Database integrity check failed!"
    exit 1
fi
```

### Restore from SQL Dump

```bash
#!/bin/bash
# restore-from-dump.sh

DUMP_FILE="/var/backups/fune/fune_export_20250107.sql.gz"
DB_PATH="./queue.db"

# Stop service
systemctl stop fune-server

# Backup current database
if [ -f "$DB_PATH" ]; then
    mv "$DB_PATH" "${DB_PATH}.pre-restore.$(date +%Y%m%d_%H%M%S)"
fi

# Create new database from dump
gunzip -c "$DUMP_FILE" | sqlite3 "$DB_PATH"

# Verify and start
sqlite3 "$DB_PATH" "PRAGMA integrity_check;"
systemctl start fune-server
```

### Point-in-Time Recovery

If you have continuous backups, you can restore to a specific point in time:

```bash
#!/bin/bash
# point-in-time-restore.sh

BACKUP_DIR="/var/backups/fune"
TARGET_DATE="2025-01-07"
TARGET_TIME="14:30:00"

# Find closest backup before target time
BACKUP_FILE=$(find "$BACKUP_DIR" -name "queue_backup_${TARGET_DATE}*.db.gz" | \
              awk -F'_' -v time="$TARGET_TIME" '{
                  gsub(/[^0-9]/, "", $4);
                  if ($4 <= time) print $0
              }' | sort | tail -1)

if [ -z "$BACKUP_FILE" ]; then
    echo "No backup found for target time"
    exit 1
fi

echo "Restoring from: $BACKUP_FILE"
# Use restore procedure from above
```

---

## Disaster Recovery

### Scenario 1: Database Corruption

**Symptoms**:
- SQLite errors in logs
- `PRAGMA integrity_check` fails
- Service crashes on startup

**Recovery Steps**:

1. Stop the service:
   ```bash
   systemctl stop fune-server
   ```

2. Try to recover data:
   ```bash
   # Attempt to recover readable data
   sqlite3 queue.db ".recover" | sqlite3 recovered.db
   ```

3. If recovery fails, restore from latest backup:
   ```bash
   ./restore-fune.sh /var/backups/fune/queue_backup_latest.db.gz
   ```

4. Analyze message loss:
   ```bash
   ./fune-admin queue -db recovered.db
   ./fune-admin queue -db queue.db
   # Compare counts to determine lost messages
   ```

### Scenario 2: Accidental Data Deletion

**Symptoms**:
- Messages accidentally purged
- Wrong status update

**Recovery Steps**:

1. Stop accepting new messages (circuit breaker or firewall):
   ```bash
   # Block HTTP port temporarily
   iptables -A INPUT -p tcp --dport 8080 -j DROP
   ```

2. Restore from most recent backup:
   ```bash
   ./restore-fune.sh /var/backups/fune/queue_backup_latest.db.gz
   ```

3. Merge recovered data with current (if needed):
   ```bash
   # Export messages from backup
   sqlite3 queue_backup.db "SELECT * FROM messages WHERE status='queued';" > recovered_messages.sql

   # Import to current database
   sqlite3 queue.db < recovered_messages.sql
   ```

4. Re-enable traffic:
   ```bash
   iptables -D INPUT -p tcp --dport 8080 -j DROP
   ```

### Scenario 3: Complete Server Loss

**Symptoms**:
- Hardware failure
- Server destroyed
- Need to rebuild from scratch

**Recovery Steps**:

1. Provision new server and install Fune

2. Restore configuration:
   ```bash
   # Restore config from version control or backup
   cp /backup/config.toml ./config.toml
   ```

3. Restore database:
   ```bash
   # Download from S3 or backup server
   aws s3 cp s3://my-bucket/fune-backups/queue_backup_latest.db.gz .

   # Restore
   gunzip queue_backup_latest.db.gz
   mv queue_backup_latest.db queue.db
   ```

4. Verify and start:
   ```bash
   ./fune-admin config
   ./fune-admin queue
   systemctl start fune-server
   ```

5. Monitor delivery resumption:
   ```bash
   tail -f /var/log/fune/fune.log
   ./fune-admin throughput
   ```

---

## Monitoring and Verification

### Backup Health Checks

Create a monitoring script to verify backup health:

```bash
#!/bin/bash
# check-backups.sh

BACKUP_DIR="/var/backups/fune"
MAX_AGE_HOURS=12

# Check if recent backup exists
LATEST_BACKUP=$(find "$BACKUP_DIR" -name "queue_backup_*.db.gz" -mtime -1 | sort | tail -1)

if [ -z "$LATEST_BACKUP" ]; then
    echo "CRITICAL: No backup found in last 24 hours"
    exit 2
fi

# Check backup age
BACKUP_AGE=$(( ($(date +%s) - $(stat -f %m "$LATEST_BACKUP")) / 3600 ))

if [ $BACKUP_AGE -gt $MAX_AGE_HOURS ]; then
    echo "WARNING: Latest backup is $BACKUP_AGE hours old"
    exit 1
fi

# Verify backup integrity
TEMP_DIR=$(mktemp -d)
gunzip -c "$LATEST_BACKUP" > "$TEMP_DIR/test.db"
sqlite3 "$TEMP_DIR/test.db" "PRAGMA integrity_check;" > /dev/null 2>&1

if [ $? -eq 0 ]; then
    echo "OK: Backup is healthy and $BACKUP_AGE hours old"
    rm -rf "$TEMP_DIR"
    exit 0
else
    echo "CRITICAL: Backup integrity check failed"
    rm -rf "$TEMP_DIR"
    exit 2
fi
```

**Integrate with monitoring**:
```cron
# Check backup health every hour
0 * * * * /path/to/check-backups.sh | logger -t fune-backup-check
```

### Backup Size Monitoring

Monitor backup size trends to detect anomalies:

```bash
#!/bin/bash
# backup-size-check.sh

BACKUP_DIR="/var/backups/fune"
LATEST_BACKUP=$(find "$BACKUP_DIR" -name "queue_backup_*.db.gz" | sort | tail -1)
PREVIOUS_BACKUP=$(find "$BACKUP_DIR" -name "queue_backup_*.db.gz" | sort | tail -2 | head -1)

LATEST_SIZE=$(stat -f %z "$LATEST_BACKUP")
PREVIOUS_SIZE=$(stat -f %z "$PREVIOUS_BACKUP")

# Alert if size increased by >200%
GROWTH=$(( $LATEST_SIZE * 100 / $PREVIOUS_SIZE ))

if [ $GROWTH -gt 200 ]; then
    echo "WARNING: Backup size increased by ${GROWTH}% (possible queue buildup)"
fi
```

---

## Production Best Practices

### 1. Backup Frequency

**Recommended schedule**:
- **High-volume** (>10k msgs/day): Every 4 hours
- **Medium-volume** (1k-10k msgs/day): Every 6 hours
- **Low-volume** (<1k msgs/day): Daily

### 2. Retention Policy

```
├── Hourly backups: Keep last 48 hours (12 backups)
├── Daily backups: Keep last 30 days (30 backups)
├── Weekly backups: Keep last 12 weeks (12 backups)
└── Monthly backups: Keep last 12 months (12 backups)
```

**Implementation**:
```bash
#!/bin/bash
# retention-policy.sh

BACKUP_DIR="/var/backups/fune"

# Keep last 48 hours of hourly backups
find "$BACKUP_DIR/hourly" -name "*.db.gz" -mtime +2 -delete

# Keep last 30 days of daily backups
find "$BACKUP_DIR/daily" -name "*.db.gz" -mtime +30 -delete

# Keep last 12 weeks of weekly backups
find "$BACKUP_DIR/weekly" -name "*.db.gz" -mtime +84 -delete

# Keep last 12 months of monthly backups
find "$BACKUP_DIR/monthly" -name "*.db.gz" -mtime +365 -delete
```

### 3. Off-Site Storage

Always maintain off-site backups:

**Option 1: S3/Object Storage**
```bash
# Upload to S3 with encryption
aws s3 cp queue_backup.db.gz \
    s3://my-bucket/fune-backups/ \
    --storage-class STANDARD_IA \
    --server-side-encryption AES256
```

**Option 2: Rsync to Remote Server**
```bash
# Sync to backup server
rsync -avz --delete \
    /var/backups/fune/ \
    backup-server:/backups/fune/
```

**Option 3: Restic (encrypted backups)**
```bash
# Initialize repository (once)
restic -r s3:s3.amazonaws.com/my-bucket/fune init

# Backup
restic -r s3:s3.amazonaws.com/my-bucket/fune \
    backup /var/backups/fune/queue.db

# Restore
restic -r s3:s3.amazonaws.com/my-bucket/fune \
    restore latest --target /restore/
```

### 4. Test Restores Regularly

**Monthly restore test**:
```bash
#!/bin/bash
# test-restore.sh

TEST_DIR="/tmp/fune-restore-test"
LATEST_BACKUP=$(find /var/backups/fune -name "*.db.gz" | sort | tail -1)

mkdir -p "$TEST_DIR"
gunzip -c "$LATEST_BACKUP" > "$TEST_DIR/test.db"

# Verify database
sqlite3 "$TEST_DIR/test.db" "PRAGMA integrity_check;"

# Query data
echo "Messages in backup:"
sqlite3 "$TEST_DIR/test.db" "SELECT COUNT(*) FROM messages;"

# Cleanup
rm -rf "$TEST_DIR"

echo "Restore test completed successfully"
```

### 5. Document Recovery Time Objective (RTO)

**Example RTO targets**:
- Database corruption: < 15 minutes
- Server failure: < 1 hour
- Data center loss: < 4 hours

### 6. Automated Alerting

Configure alerts for backup failures:

```bash
#!/bin/bash
# backup-with-alert.sh

./backup-fune.sh

if [ $? -ne 0 ]; then
    # Send alert (example: email, Slack, PagerDuty)
    curl -X POST https://hooks.slack.com/services/YOUR/WEBHOOK/URL \
        -H 'Content-Type: application/json' \
        -d '{"text":"❌ Fune backup failed!"}'
fi
```

---

## Additional Resources

- [SQLite Backup Documentation](https://www.sqlite.org/backup.html)
- [SQLite WAL Mode](https://www.sqlite.org/wal.html)
- Fune Admin Tool: `./fune-admin --help`
- Configuration: [config.toml.example](../config.toml.example)

---

## Quick Reference

```bash
# Backup
sqlite3 queue.db ".backup 'backup.db'"

# Restore
systemctl stop fune-server
mv queue.db queue.db.old
cp backup.db queue.db
systemctl start fune-server

# Verify
sqlite3 queue.db "PRAGMA integrity_check;"

# Check queue
./fune-admin queue

# Monitor
./fune-admin throughput
```

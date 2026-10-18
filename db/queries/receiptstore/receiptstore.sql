-- name: StoreAutomaticRotationNeeded :one
-- Used by automaticRotationNeeded in automatic_rotation_linux.go.
SELECT count(*),COALESCE(bool_or(l.reserved_records<l.record_ceiling AND l.reserved_bytes<=l.byte_ceiling-$3::bigint),false)::boolean FROM fry_member_file_lane l JOIN fry_member_file f ON f.member=l.member AND f.file_id=l.file_id WHERE l.member=$1 AND l.file_id=$2 AND f.state='current';

-- name: PackMembers :many
-- Used by packMembers in compaction_linux.go.
SELECT receipt FROM fry_pack_member WHERE pack=$1 ORDER BY receipt;

-- name: LockPacks :one
-- Used by lockPacks in compaction_linux.go.
SELECT id FROM fry_pack WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: PackerRegisterRewriteReadPack :one
-- Used by registerRewrite in compaction_linux.go.
SELECT state FROM fry_pack WHERE id=$1 AND organization=$2;

-- name: PackerRegisterRewriteReadReceipt :one
-- Used by registerRewrite in compaction_linux.go.
SELECT pack_id,manifest_version FROM fry_receipt WHERE id=$1;

-- name: PackerRegisterRewriteReadPackState :one
-- Used by registerRewrite in compaction_linux.go.
SELECT state FROM fry_pack WHERE id=$1;

-- name: PackerRegisterRewriteUpdateReceipt :execresult
-- Used by registerRewrite in compaction_linux.go.
UPDATE fry_receipt SET pack_id=NULL,manifest_version=manifest_version+1 WHERE pack_id=$1;

-- name: PackerRegisterRewriteUpdatePack :execresult
-- Used by registerRewrite in compaction_linux.go.
UPDATE fry_pack SET state='retiring',epoch=epoch+1 WHERE id=$1;

-- name: PackerRegisterRewriteInsertPack :execresult
-- Used by registerRewrite in compaction_linux.go.
INSERT INTO fry_pack(id,organization,object_key,state,lease_until,source_pack,staging_owner) VALUES($1,$2,$3,'building',clock_timestamp()+$4::bigint*interval '1 microsecond',$5,$6);

-- name: PackerRegisterRewriteInsertPackMember :execresult
-- Used by registerRewrite in compaction_linux.go.
INSERT INTO fry_pack_member(pack,receipt,manifest_version) VALUES($1,$2,$3);

-- name: PackerPublishRewriteReadReference :one
-- Used by publishRewrite in compaction_linux.go.
SELECT pack_id,manifest_version,EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())) FROM fry_receipt r WHERE r.id=$1;

-- name: PackerPublishRewriteReadPack :one
-- Used by publishRewrite in compaction_linux.go.
SELECT state FROM fry_pack WHERE id=$1;

-- name: PackerPublishRewriteReadPackImageSizeImageSha256 :one
-- Used by publishRewrite in compaction_linux.go.
SELECT image_size,image_sha256 FROM fry_pack WHERE id=$1;

-- name: PackerPublishRewriteUpdateReceipt :execresult
-- Used by publishRewrite in compaction_linux.go.
UPDATE fry_receipt SET pack_id=$2,manifest_version=manifest_version+1 WHERE id=$1;

-- name: PackerPublishRewriteUpdatePack :execresult
-- Used by publishRewrite in compaction_linux.go.
UPDATE fry_pack SET state='retiring',epoch=epoch+1 WHERE id=$1;

-- name: PackerPublishRewriteUpdatePackPublished :execresult
-- Used by publishRewrite in compaction_linux.go.
UPDATE fry_pack SET state='published',etag=$2 WHERE id=$1;

-- name: StoreLoadHeaderMetadata :one
-- Used by loadHeaderMetadata in header_metadata_linux.go.
SELECT r.raw_size,h.raw_header,
 r.lifecycle='live' AND EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp()))
 FROM fry_receipt r LEFT JOIN fry_header_metadata h ON h.receipt=r.id WHERE r.id=$1 AND r.organization=$2;

-- name: StoreStoreHeaderMetadataReadHeaderMetadata :one
-- Used by storeHeaderMetadata in header_metadata_linux.go.
SELECT raw_header FROM fry_header_metadata WHERE receipt=$1;

-- name: StoreStoreHeaderMetadataInsertHeaderMetadata :execresult
-- Used by storeHeaderMetadata in header_metadata_linux.go.
INSERT INTO fry_header_metadata(receipt,raw_header) VALUES($1,$2);

-- name: LocalMemberReconcileRewritesReadHotRewrite :many
-- Used by reconcileRewrites in hot_cleanup_linux.go.
SELECT id,old_incarnation,old_directory,new_directory,state,new_incarnation,kind FROM fry_hot_rewrite WHERE member=$1 AND state IN ('building','published') ORDER BY id;

-- name: LocalMemberReconcileRewritesUpdateMemberFileLane :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_member_file_lane SET reserved_records=0,reserved_bytes=0 WHERE member=$1 AND file_id=$2;

-- name: LocalMemberReconcileRewritesUpdateMemberFile :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_member_file SET state='removed',charged_bytes=0 WHERE member=$1 AND file_id=$2 AND state='building' AND rewrite_job=$3;

-- name: LocalMemberReconcileRewritesUpdateHotRewrite :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_hot_rewrite SET state='abandoned',reserved_bytes=0 WHERE id=$1 AND state='building';

-- name: LocalMemberReconcileRewritesUpdateHotRewriteComplete :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_hot_rewrite SET state='complete' WHERE id=$1 AND member=$2 AND kind='rotate' AND state='published';

-- name: LocalMemberReconcileRewritesUpdateTarget :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_target SET bytes_removed=true WHERE member=$1 AND incarnation=$2;

-- name: LocalMemberReconcileRewritesUpdateCleanup :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_cleanup c SET bytes_removed=true FROM fry_target t WHERE c.attempt=t.attempt AND c.member=t.member AND t.member=$1 AND t.incarnation=$2 AND t.bytes_removed;

-- name: LocalMemberReconcileRewritesUpdateMemberFileLane2 :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_member_file_lane SET reserved_records=0,reserved_bytes=0 WHERE member=$1 AND file_id=$2;

-- name: LocalMemberReconcileRewritesUpdateMemberFileRemoved :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_member_file SET state='removed',charged_bytes=0 WHERE member=$1 AND file_id=$2 AND state='retiring' AND rewrite_job=$3;

-- name: LocalMemberReconcileRewritesUpdateHotRewriteRemoved :execresult
-- Used by reconcileRewrites in hot_cleanup_linux.go.
UPDATE fry_hot_rewrite SET state='removed' WHERE id=$1 AND state='published';

-- name: StoreSnapshotHotFileReadTarget :many
-- Used by snapshotHotFile in hot_cleanup_linux.go.
SELECT a.id,a.receipt_id,a.state,t.capacity_lane FROM fry_target t JOIN fry_attempt a ON a.id=t.attempt WHERE t.member=$1 AND t.incarnation=$2 AND NOT t.bytes_removed ORDER BY a.id FOR UPDATE OF a;

-- name: StoreSnapshotHotFileReadTargetCancelConfirmed :one
-- Used by snapshotHotFile in hot_cleanup_linux.go.
SELECT cancel_confirmed FROM fry_target WHERE attempt=$1 AND member=$2;

-- name: StoreSnapshotHotFileReadReceipt :one
-- Used by snapshotHotFile in hot_cleanup_linux.go.
SELECT organization FROM fry_receipt WHERE id=$1 FOR UPDATE;

-- name: StoreSnapshotHotFileReadReadPin :one
-- Used by snapshotHotFile in hot_cleanup_linux.go.
SELECT manifest_version,(lifecycle='live' AND pack_id IS NULL) OR EXISTS(SELECT 1 FROM fry_read_pin WHERE receipt=r.id) FROM fry_receipt r WHERE r.id=$1;

-- name: StoreSnapshotHotFileReadTargetLocator :one
-- Used by snapshotHotFile in hot_cleanup_linux.go.
SELECT locator FROM fry_target WHERE attempt=$1 AND member=$2;

-- name: StoreRewriteHotInsertHotRewrite :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
INSERT INTO fry_hot_rewrite(id,member,old_incarnation,old_directory,new_directory,state,reserved_bytes) VALUES($1,$2,$3,$4,$5,'building',$6);

-- name: StoreRewriteHotReadMember :one
-- Used by rewriteHot in hot_cleanup_linux.go.
SELECT incarnation,maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: StoreRewriteHotUpdateTarget :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_target SET incarnation=$3,locator=$4 WHERE attempt=$1 AND member=$2;

-- name: StoreRewriteHotUpdateReceipt :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_receipt SET manifest_version=manifest_version+1 WHERE id=$1;

-- name: StoreRewriteHotUpdateMemberFile :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_member_file SET state='retiring',rewrite_job=$3,charged_bytes=CASE WHEN charged_bytes>0 THEN $5 ELSE 0 END WHERE member=$1 AND file_id=$2 AND directory=$4 AND state='current' AND rewrite_job IS NULL;

-- name: StoreRewriteHotUpdateMemberFileCurrent :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_member_file SET state='current',rewrite_job=NULL WHERE member=$1 AND file_id=$2 AND directory=$4 AND state='building' AND rewrite_job=$3;

-- name: StoreRewriteHotUpdateMember :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_member SET incarnation=$2,directory=$3 WHERE id=$1;

-- name: StoreRewriteHotUpdateHotRewrite :execresult
-- Used by rewriteHot in hot_cleanup_linux.go.
UPDATE fry_hot_rewrite SET state='published' WHERE id=$1 AND state='building';

-- name: StoreFinishCleanupDeleteHeaderMetadata :execresult
-- Used by FinishCleanup in hot_cleanup_linux.go.
DELETE FROM fry_header_metadata WHERE receipt=$1 AND EXISTS(SELECT 1 FROM fry_receipt WHERE fry_receipt.id=$1 AND lifecycle='retiring');

-- name: StoreFinishCleanupUpdateReceipt :one
-- Used by FinishCleanup in hot_cleanup_linux.go.
UPDATE fry_receipt r SET lifecycle='removed',removed_at=clock_timestamp() WHERE r.id=$1 AND lifecycle='retiring'
    AND NOT EXISTS(SELECT 1 FROM fry_target WHERE attempt=r.attempt AND NOT bytes_removed)
    AND NOT EXISTS(SELECT 1 FROM fry_pack_member m JOIN fry_pack p ON p.id=m.pack WHERE m.receipt=r.id AND (p.state!='retired' OR NOT p.staging_closed))
    AND NOT EXISTS(SELECT 1 FROM fry_read_pin WHERE receipt=r.id) RETURNING organization,raw_size;

-- name: StoreSelectCleanupCandidateReadReceipt :one
-- Used by selectCleanupCandidate in hot_cleanup_linux.go.
SELECT cleanup_due,id FROM fry_receipt WHERE lifecycle='retiring' ORDER BY cleanup_due DESC NULLS FIRST,id DESC LIMIT 1;

-- name: StoreSelectCleanupCandidateReadReceiptRetiring :one
-- Used by selectCleanupCandidate in hot_cleanup_linux.go.
SELECT id,organization,cleanup_due FROM fry_receipt WHERE lifecycle='retiring' AND (cleanup_due,id)<=($1::timestamptz,$2::text) ORDER BY cleanup_due,id LIMIT 1;

-- name: StoreSelectCleanupCandidateReadReceiptRetiringIdOrganizationCleanupDue :one
-- Used by selectCleanupCandidate in hot_cleanup_linux.go.
SELECT id,organization,cleanup_due FROM fry_receipt WHERE lifecycle='retiring' AND (cleanup_due,id)>($1::timestamptz,$2::text) AND (cleanup_due,id)<=($3::timestamptz,$4::text) ORDER BY cleanup_due,id LIMIT 1;

-- name: LockReceipt :one
-- Used by lockReceipt in lifecycle_linux.go.
SELECT lifecycle FROM fry_receipt WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: StoreReconcileLifetimeCheckReference :one
-- Used by reconcileLifetime in lifecycle_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_reference WHERE fry_reference.receipt=$1 AND (expires_at IS NULL OR expires_at>clock_timestamp()));

-- name: StoreReconcileLifetimeUpdateReceipt :execresult
-- Used by reconcileLifetime in lifecycle_linux.go.
UPDATE fry_receipt SET lifecycle='retiring',manifest_version=manifest_version+1,
 eligible_at=COALESCE((SELECT max(expires_at) FROM fry_reference WHERE fry_reference.receipt=$1),clock_timestamp()),
 cleanup_due=COALESCE((SELECT max(expires_at) FROM fry_reference WHERE fry_reference.receipt=$1),clock_timestamp())+(SELECT cleanup_microseconds FROM fry_attempt WHERE receipt_id=$1)*interval '1 microsecond'
 WHERE id=$1 AND lifecycle='live';

-- name: StoreAddReferenceCheckReferenceRelease :one
-- Used by AddReference in lifecycle_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_reference_release WHERE receipt=$1 AND kind=$2 AND reference_key=$3);

-- name: StoreAddReferenceReadClock :one
-- Used by AddReference in lifecycle_linux.go.
SELECT clock_timestamp()::timestamptz;

-- name: StoreAddReferenceInsertReference :execresult
-- Used by AddReference in lifecycle_linux.go.
INSERT INTO fry_reference(receipt,kind,reference_key,expires_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING;

-- name: StoreAddReferenceReadSqlc :one
-- Used by AddReference in lifecycle_linux.go.
SELECT expires_at IS NOT DISTINCT FROM sqlc.narg('expires_at')::timestamptz FROM fry_reference WHERE fry_reference.receipt=$1 AND kind=$2 AND reference_key=$3;

-- name: StoreReleaseReferenceInsertReferenceRelease :execresult
-- Used by ReleaseReference in lifecycle_linux.go.
INSERT INTO fry_reference_release(receipt,kind,reference_key) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: StoreReleaseReferenceUpdateReference :execresult
-- Used by ReleaseReference in lifecycle_linux.go.
UPDATE fry_reference SET expires_at=LEAST(COALESCE(expires_at,clock_timestamp()),clock_timestamp()) WHERE receipt=$1 AND kind=$2 AND reference_key=$3;

-- name: StoreSweepExpiry :many
-- Used by SweepExpiry in lifecycle_linux.go.
SELECT id,organization FROM fry_receipt r WHERE lifecycle='live' AND NOT EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())) ORDER BY id LIMIT $1;

-- name: StoreCleanupStatus :one
-- Used by CleanupStatus in lifecycle_linux.go.
SELECT lifecycle,eligible_at,cleanup_due,removed_at,
 (SELECT count(*) FROM fry_target WHERE attempt=r.attempt AND NOT bytes_removed),
 (SELECT count(*) FROM fry_pack_member m JOIN fry_pack p ON p.id=m.pack WHERE m.receipt=r.id AND (p.state!='retired' OR NOT p.staging_closed)),
 (SELECT count(*) FROM fry_read_pin WHERE receipt=r.id),COALESCE(cleanup_due<clock_timestamp() AND lifecycle!='removed',false)::boolean,COALESCE(cleanup_due<COALESCE(removed_at,clock_timestamp()),false)::boolean
 FROM fry_receipt r WHERE r.id=$1 AND r.organization=$2;

-- name: StoreOpenReadReceipt :one
-- Used by Open in lifecycle_linux.go.
SELECT raw_size,pack_id FROM fry_receipt WHERE id=$1;

-- name: StoreOpenReadPack :one
-- Used by Open in lifecycle_linux.go.
SELECT p.state,p.object_key,p.etag,m.locator FROM fry_pack p JOIN fry_pack_member m ON m.pack=p.id WHERE p.id=$1 AND m.receipt=$2 FOR SHARE OF p;

-- name: StoreOpenInsertReadPin :execresult
-- Used by Open in lifecycle_linux.go.
INSERT INTO fry_read_pin(id,receipt,pack,deadline) VALUES($1,$2,$3,clock_timestamp()+$4::bigint*interval '1 microsecond');

-- name: StoreOpenDeleteReadPin :execresult
-- Used by Open in lifecycle_linux.go.
DELETE FROM fry_read_pin WHERE id=$1;

-- name: EnsureInboxInTransactionInsertInbox :execresult
-- Used by EnsureInboxInTransaction in mailbox_linux.go.
INSERT INTO fry_inbox(id,organization) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: EnsureInboxInTransactionReadInbox :one
-- Used by EnsureInboxInTransaction in mailbox_linux.go.
SELECT organization FROM fry_inbox WHERE id=$1 AND migration_job IS NULL FOR UPDATE;

-- name: EnsureInboxInTransactionInsertFolder :execresult
-- Used by EnsureInboxInTransaction in mailbox_linux.go.
INSERT INTO fry_folder(id,inbox,name) VALUES($1,$2,'INBOX') ON CONFLICT(inbox,name) DO NOTHING;

-- name: EnsureInboxInTransactionInsertSubscription :execresult
-- Used by EnsureInboxInTransaction in mailbox_linux.go.
INSERT INTO fry_subscription(inbox,name) VALUES($1,'INBOX') ON CONFLICT DO NOTHING;

-- name: StoreFolders :many
-- Used by Folders in mailbox_linux.go.
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE i.id=$1 AND i.organization=$2 AND i.migration_job IS NULL ORDER BY f.name;

-- name: StoreCreateFolderReadInbox :one
-- Used by CreateFolder in mailbox_linux.go.
SELECT id FROM fry_inbox WHERE id=$1 AND organization=$2 AND migration_job IS NULL FOR UPDATE;

-- name: StoreCreateFolderInsertFolder :execresult
-- Used by CreateFolder in mailbox_linux.go.
INSERT INTO fry_folder(id,inbox,name) VALUES($1,$2,$3) ON CONFLICT(inbox,name) DO NOTHING;

-- name: StoreSubscribeFolderInsertSubscription :execresult
-- Used by SubscribeFolder in mailbox_linux.go.
INSERT INTO fry_subscription(inbox,name) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: StoreSubscribeFolderDeleteSubscription :execresult
-- Used by SubscribeFolder in mailbox_linux.go.
DELETE FROM fry_subscription WHERE inbox=$1 AND name=$2;

-- name: StorePublishMailboxes :execresult
-- Used by PublishMailboxes in mailbox_linux.go.
UPDATE fry_work SET available_at=clock_timestamp()+interval '1 minute',failures=failures+1,last_error=$3 WHERE receipt=$1 AND inbox=$2;

-- name: StoreSelectMailboxTurnReadClock :one
-- Used by selectMailboxTurn in mailbox_linux.go.
SELECT clock_timestamp()::timestamptz;

-- name: StoreSelectMailboxTurnReadWork :many
-- Used by selectMailboxTurn in mailbox_linux.go.
SELECT w.receipt,w.inbox,w.organization FROM fry_work w WHERE w.organization=$1 AND w.available_at<=$2 AND EXISTS (SELECT 1 FROM fry_receipt r JOIN fry_inbox i ON i.id=w.inbox AND i.organization=r.organization WHERE r.id=w.receipt AND r.organization=w.organization AND i.migration_job IS NULL) ORDER BY w.available_at,w.received_at,w.receipt,w.inbox LIMIT $3;

-- name: PublishMailboxInTransactionCheckWork :one
-- Used by publishMailboxInTransaction in mailbox_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_work WHERE receipt=$1 AND inbox=$2);

-- name: PublishMailboxInTransactionCheckReference :one
-- Used by publishMailboxInTransaction in mailbox_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_reference WHERE fry_reference.receipt=$1 AND kind='pending-mailbox' AND reference_key=$2 AND (expires_at IS NULL OR expires_at>clock_timestamp()));

-- name: PublishMailboxInTransactionReadWork :one
-- Used by publishMailboxInTransaction in mailbox_linux.go.
SELECT folder,flags,internal_date FROM fry_work WHERE receipt=$1 AND inbox=$2;

-- name: PublishMailboxInTransactionReadSubscription :one
-- Used by publishMailboxInTransaction in mailbox_linux.go.
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE f.id=$1 AND i.id=$2 AND i.organization=$3 AND (i.migration_job IS NULL OR $4::boolean) FOR UPDATE OF f;

-- name: PublishMailboxInTransactionInsertMail :execresult
-- Used by publishMailboxInTransaction in mailbox_linux.go.
INSERT INTO fry_mail(id,folder,uid,receipt,internal_date,flags) SELECT sqlc.arg('id'),sqlc.arg('folder'),sqlc.arg('uid')::bigint,r.id,COALESCE(sqlc.narg('internal_date')::timestamptz,r.received_at),sqlc.arg('flags')::text[] FROM fry_receipt r WHERE r.id=sqlc.arg('receipt');

-- name: PublishMailboxInTransactionUpdateFolder :execresult
-- Used by publishMailboxInTransaction in mailbox_linux.go.
UPDATE fry_folder SET uidnext=uidnext+1 WHERE id=$1;

-- name: PublishMailboxInTransactionInsertReference :execresult
-- Used by publishMailboxInTransaction in mailbox_linux.go.
INSERT INTO fry_reference(receipt,kind,reference_key) VALUES($1,'mailbox',$2);

-- name: PublishMailboxInTransactionDeleteWork :execresult
-- Used by publishMailboxInTransaction in mailbox_linux.go.
DELETE FROM fry_work WHERE receipt=$1 AND inbox=$2;

-- name: ReleaseMailboxRefInsertReferenceRelease :execresult
-- Used by releaseMailboxRef in mailbox_linux.go.
INSERT INTO fry_reference_release(receipt,kind,reference_key) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: ReleaseMailboxRefUpdateReference :execresult
-- Used by releaseMailboxRef in mailbox_linux.go.
UPDATE fry_reference SET expires_at=LEAST(COALESCE(expires_at,clock_timestamp()),clock_timestamp()) WHERE receipt=$1 AND kind=$2 AND reference_key=$3;

-- name: MailboxRows :many
-- Used by mailboxRows in mailbox_linux.go.
SELECT m.id,m.receipt,m.uid,m.flags,m.internal_date,r.raw_size FROM fry_mail m JOIN fry_receipt r ON r.id=m.receipt WHERE folder=$1 ORDER BY uid;

-- name: StoreExpunge :execresult
-- Used by expunge in mailbox_linux.go.
DELETE FROM fry_mail WHERE id=$1;

-- name: StoreCopyMail :one
-- Used by copyMail in mailbox_linux.go.
SELECT id FROM fry_folder WHERE id=$1 FOR UPDATE;

-- name: CopyMembershipsInsertMail :execresult
-- Used by copyMemberships in mailbox_linux.go.
INSERT INTO fry_mail(id,folder,uid,receipt,flags,internal_date) VALUES($1,$2,$3,$4,$5,$6);

-- name: CopyMembershipsInsertReference :execresult
-- Used by copyMemberships in mailbox_linux.go.
INSERT INTO fry_reference(receipt,kind,reference_key) VALUES($1,'mailbox',$2);

-- name: CopyMembershipsDeleteMail :execresult
-- Used by copyMemberships in mailbox_linux.go.
DELETE FROM fry_mail WHERE id=$1;

-- name: CopyMembershipsUpdateFolder :execresult
-- Used by copyMemberships in mailbox_linux.go.
UPDATE fry_folder SET uidnext=$1 WHERE id=$2;

-- name: StoreDeleteFolderDeleteMail :execresult
-- Used by DeleteFolder in mailbox_linux.go.
DELETE FROM fry_mail WHERE folder=$1;

-- name: StoreDeleteFolderDeleteFolder :execresult
-- Used by DeleteFolder in mailbox_linux.go.
DELETE FROM fry_folder WHERE id=$1;

-- name: StoreAppended :one
-- Used by Appended in mailbox_linux.go.
SELECT a.folder,a.uidvalidity,a.uid FROM fry_append_result a JOIN fry_receipt r ON r.id=a.receipt WHERE r.organization=$1 AND r.id=$2;

-- name: RegisterKeywords :execresult
-- Used by registerKeywords in mailbox_linux.go.
UPDATE fry_folder SET keywords=ARRAY(SELECT DISTINCT value FROM unnest(keywords || $2::text[]) AS value ORDER BY value) WHERE id=$1 AND NOT($2::text[] <@ keywords);

-- name: MaintenanceOversized :one
-- Used by oversized in maintenance_linux.go.
SELECT count(*) FROM (SELECT r.id FROM fry_receipt r WHERE r.lifecycle='live' AND r.pack_id IS NULL
 AND EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM fry_pack_member pm JOIN fry_pack p ON p.id=pm.pack WHERE pm.receipt=r.id AND p.state!='retired') AND r.raw_size>$1 LIMIT $2) oversized;

-- name: MaintenancePack :many
-- Used by pack in maintenance_linux.go.
SELECT r.id,r.raw_size,a.recovery_seconds FROM fry_receipt r JOIN fry_attempt a ON a.id=r.attempt WHERE r.lifecycle='live' AND r.pack_id IS NULL
 AND EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM fry_pack_member pm JOIN fry_pack p ON p.id=pm.pack WHERE pm.receipt=r.id AND p.state!='retired') AND r.raw_size<=$1 AND r.organization=$2 ORDER BY r.received_at,r.id LIMIT $3;

-- name: MaintenanceCompact :one
-- Used by compact in maintenance_linux.go.
SELECT p.id FROM fry_pack p WHERE p.state='published' AND p.staging_closed
 AND EXISTS(SELECT 1 FROM fry_pack_member pm JOIN fry_receipt r ON r.id=pm.receipt WHERE pm.pack=p.id AND r.pack_id=p.id
  AND NOT EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())))
 AND NOT EXISTS(SELECT 1 FROM fry_pack pending WHERE pending.source_pack=p.id AND pending.state IN ('building','retiring')) AND p.organization=$1 ORDER BY p.id LIMIT 1;

-- name: MaintenanceRetire :one
-- Used by retire in maintenance_linux.go.
SELECT p.id FROM fry_pack p WHERE p.staging_closed AND (p.state='retiring' OR (p.state='building' AND p.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM fry_read_pin pin WHERE pin.pack=p.id) AND p.organization=$1 ORDER BY p.id LIMIT 1;

-- name: PreflightMemberProfileCheckMember :one
-- Used by preflightMemberProfile in member_capacity_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member WHERE id=$1);

-- name: PreflightMemberProfileReadMemberProfile :one
-- Used by preflightMemberProfile in member_capacity_linux.go.
SELECT enabled,max_message_bytes,max_concurrent_appends,max_bytes,max_records,max_file_bytes,staging_bytes,lane_version,lane_count FROM fry_member_profile WHERE member=$1;

-- name: PreflightMemberProfileReadMemberFile :one
-- Used by preflightMemberProfile in member_capacity_linux.go.
SELECT
 EXISTS(SELECT 1 FROM fry_member_file f WHERE f.member=$1 AND f.state IN ('current','building','retiring','sealed') AND ((f.state IN ('current','building') AND f.charged_bytes<>$2) OR (f.state IN ('retiring','sealed') AND (f.charged_bytes<64 OR f.charged_bytes>$2)))) OR
 EXISTS(SELECT 1 FROM fry_hot_rewrite j WHERE j.member=$1 AND j.state='building' AND ((j.new_incarnation IS NULL AND j.reserved_bytes<>CASE WHEN j.kind='sealed-reclaim' AND j.kept_records=0 THEN 0 ELSE $2 END) OR (j.new_incarnation IS NOT NULL AND j.reserved_bytes<>0))) OR
 (COALESCE((SELECT sum(charged_bytes) FROM fry_member_file WHERE fry_member_file.member=$1 AND state<>'removed'),0)+COALESCE((SELECT sum(reserved_bytes) FROM fry_hot_rewrite WHERE fry_hot_rewrite.member=$1 AND state='building'),0)+$3::bigint)>$4::bigint;

-- name: PreflightMemberProfileReadMemberFileRemoved :many
-- Used by preflightMemberProfile in member_capacity_linux.go.
SELECT f.file_id,l.lane,l.record_ceiling,l.byte_ceiling,l.reserved_records,l.reserved_bytes FROM fry_member_file f LEFT JOIN fry_member_file_lane l ON l.member=f.member AND l.file_id=f.file_id WHERE f.member=$1 AND f.state<>'removed' ORDER BY f.file_id,l.lane;

-- name: InsertMemberProfile :execresult
-- Used by insertMemberProfile in member_capacity_linux.go.
INSERT INTO fry_member_profile(member,enabled,max_message_bytes,max_concurrent_appends,max_bytes,max_records,max_file_bytes,staging_bytes,lane_version,lane_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9);

-- name: InsertCapacityLanes :execresult
-- Used by insertCapacityLanes in member_capacity_linux.go.
INSERT INTO fry_member_file_lane(member,file_id,lane,record_ceiling,byte_ceiling) VALUES($1,$2,$3,$4,$5);

-- name: ReserveTargetCapacityReadTarget :one
-- Used by reserveTargetCapacity in member_capacity_linux.go.
SELECT capacity_reserved_bytes,capacity_lane FROM fry_target WHERE attempt=$1 AND member=$2 AND incarnation=$3;

-- name: ReserveTargetCapacityCandidate :one
-- Used by reserveTargetCapacity in member_capacity_linux.go.
WITH candidate AS MATERIALIZED (
 SELECT l.lane FROM fry_member_file_lane l JOIN fry_member_file f ON f.member=l.member AND f.file_id=l.file_id
 WHERE l.member=$1 AND l.file_id=$2 AND f.state='current' AND l.reserved_records<l.record_ceiling AND l.reserved_bytes<=l.byte_ceiling-$3::bigint
 ORDER BY ((l.lane::bigint-$4::bigint+$5::bigint)%$5::bigint) LIMIT 1
 ), charged AS (
 UPDATE fry_member_file_lane l SET reserved_records=l.reserved_records+1,reserved_bytes=l.reserved_bytes+$3
 FROM candidate c WHERE l.member=$1 AND l.file_id=$2 AND l.lane=c.lane AND l.reserved_records<l.record_ceiling AND l.reserved_bytes<=l.byte_ceiling-$3::bigint
 RETURNING l.lane
 ), stamped AS (
 UPDATE fry_target t SET capacity_reserved_bytes=$3,capacity_lane=c.lane FROM charged c
 WHERE t.attempt=$6 AND t.member=$1 AND t.incarnation=$2 AND t.capacity_reserved_bytes IS NULL AND t.capacity_lane IS NULL AND NOT t.capacity_settled RETURNING 1
 ) SELECT EXISTS(SELECT 1 FROM candidate),(SELECT count(*) FROM charged),(SELECT count(*) FROM stamped);

-- name: SettleTargetCapacityRefunded :one
-- Used by settleTargetCapacity in member_capacity_linux.go.
WITH refunded AS (
  UPDATE fry_member_file_lane l SET reserved_bytes=l.reserved_bytes-$3+$4
  WHERE l.member=$1 AND l.file_id=$2 AND l.lane=$6 AND l.reserved_bytes>=$3 AND l.reserved_records>0
  AND EXISTS(SELECT 1 FROM fry_member_file f WHERE f.member=$1 AND f.file_id=$2 AND f.state='current')
  RETURNING l.reserved_records,l.reserved_bytes
  ), stamped AS (
  UPDATE fry_target SET capacity_reserved_bytes=$4,capacity_settled=true
  WHERE attempt=$5 AND member=$1 AND incarnation=$2 AND capacity_lane=$6 AND NOT capacity_settled AND capacity_reserved_bytes=$3
  AND EXISTS(SELECT 1 FROM refunded WHERE reserved_records>0 AND reserved_bytes>=$4) RETURNING 1
  ) SELECT (SELECT count(*) FROM refunded),(SELECT count(*) FROM stamped);

-- name: SettleTargetCapacityUpdateTarget :execresult
-- Used by settleTargetCapacity in member_capacity_linux.go.
UPDATE fry_target SET capacity_reserved_bytes=$3,capacity_settled=true WHERE fry_target.attempt=$1 AND fry_target.member=$2 AND incarnation=$4 AND capacity_lane=$6 AND NOT capacity_settled AND capacity_reserved_bytes=$5 AND EXISTS(SELECT 1 FROM fry_member_file f JOIN fry_member_file_lane l ON l.member=f.member AND l.file_id=f.file_id WHERE f.member=$2 AND f.file_id=$4 AND f.state='current' AND l.lane=$6 AND l.reserved_records>0 AND l.reserved_bytes>=$3);

-- name: ReserveRewriteCapacityReadMemberProfile :one
-- Used by reserveRewriteCapacity in member_capacity_linux.go.
SELECT enabled,max_file_bytes,staging_bytes,max_bytes FROM fry_member_profile WHERE member=$1 FOR UPDATE;

-- name: ReserveRewriteCapacityCheckMemberFile :one
-- Used by reserveRewriteCapacity in member_capacity_linux.go.
SELECT (COALESCE((SELECT sum(charged_bytes) FROM fry_member_file WHERE fry_member_file.member=$1 AND state<>'removed'),0)+COALESCE((SELECT sum(reserved_bytes) FROM fry_hot_rewrite WHERE fry_hot_rewrite.member=$1 AND state='building'),0))::bigint;

-- name: SetSuccessorCapacity :execresult
-- Used by setSuccessorCapacity in member_capacity_linux.go.
UPDATE fry_member_file_lane l SET reserved_records=$4,reserved_bytes=$5 WHERE l.member=$1 AND l.file_id=$2 AND l.lane=$3 AND $4::bigint<=l.record_ceiling AND $5::bigint<=l.byte_ceiling AND EXISTS(SELECT 1 FROM fry_member_file f WHERE f.member=$1 AND f.file_id=$2 AND f.state='building' AND f.rewrite_job=$6);

-- name: PreflightMemberFilesReadMemberFile :many
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT f.member,f.file_id,f.directory,f.state,f.sealed_records,f.rewrite_job,j.id,j.member,j.old_incarnation,j.old_directory,j.new_directory,j.state,j.kind,j.new_incarnation,j.reserved_bytes,j.kept_records,f.charged_bytes,t.capacity_reserved_bytes,t.capacity_settled,t.capacity_lane,l.member,l.file_id,l.lane,l.record_ceiling,l.byte_ceiling,l.reserved_records,l.reserved_bytes,k.kind,k.selected_file FROM fry_member_file f CROSS JOIN fry_hot_rewrite j CROSS JOIN fry_target t CROSS JOIN fry_member_file_lane l CROSS JOIN fry_member_maintenance k WHERE false;

-- name: PreflightMemberFilesReadMember :one
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT incarnation,directory FROM fry_member WHERE id=$1;

-- name: PreflightMemberFilesCheckMemberFile :one
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member_file WHERE member=$1);

-- name: PreflightMemberFilesReadMemberFileLane :one
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT file_id,directory,(64+COALESCE((SELECT sum(l.reserved_bytes) FROM fry_member_file_lane l WHERE l.member=f.member AND l.file_id=f.file_id),0))::bigint FROM fry_member_file f WHERE f.member=$1 AND state='current' AND rewrite_job IS NULL;

-- name: PreflightMemberFilesCheckMemberFileBuilding :one
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member_file f LEFT JOIN fry_hot_rewrite j ON j.id=f.rewrite_job WHERE f.member=$1 AND f.state IN ('building','retiring') AND (
 (f.state='building' AND (j.id IS NULL OR j.member<>f.member OR j.state<>'building' OR j.new_incarnation IS DISTINCT FROM f.file_id OR j.new_directory<>f.directory)) OR
 (f.state='retiring' AND (j.id IS NULL OR j.member<>f.member OR j.state<>'published' OR j.old_incarnation<>f.file_id OR j.old_directory<>f.directory))));

-- name: PreflightMemberFilesReadHotRewrite :many
-- Used by preflightMemberFiles in member_files_linux.go.
SELECT id,old_incarnation,old_directory,new_directory,state,new_incarnation,kind FROM fry_hot_rewrite WHERE member=$1 AND state IN ('building','published') ORDER BY id;

-- name: VerifyCatalogPathCheckMemberFile :one
-- Used by verifyCatalogPath in member_files_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member_file WHERE fry_member_file.member=$1 AND fry_member_file.directory=$2) OR EXISTS(SELECT 1 FROM fry_member WHERE fry_member.id=$1 AND fry_member.directory=$2);

-- name: VerifyCatalogPathReadMemberFile :one
-- Used by verifyCatalogPath in member_files_linux.go.
SELECT f.charged_bytes,p.enabled FROM fry_member_file f JOIN fry_member_profile p ON p.member=f.member WHERE f.member=$1 AND f.file_id=$2 AND f.directory=$3 AND f.state=$4 AND f.rewrite_job=$5;

-- name: LocalMemberRegisterSuccessorUpdateHotRewrite :execresult
-- Used by registerSuccessor in member_files_linux.go.
UPDATE fry_hot_rewrite SET new_incarnation=$3 WHERE id=$1 AND member=$2 AND state='building' AND new_incarnation IS NULL AND new_directory=$4;

-- name: LocalMemberRegisterSuccessorInsertMemberFile :execresult
-- Used by registerSuccessor in member_files_linux.go.
INSERT INTO fry_member_file(member,file_id,directory,state,rewrite_job,charged_bytes) SELECT $1,$2,$3,'building',$4,j.reserved_bytes FROM fry_hot_rewrite j JOIN fry_member_profile p ON p.member=j.member WHERE j.id=$4 AND ((p.enabled AND j.reserved_bytes=p.max_file_bytes) OR (NOT p.enabled AND j.reserved_bytes=0));

-- name: LocalMemberRegisterSuccessorUpdateHotRewrite2 :execresult
-- Used by registerSuccessor in member_files_linux.go.
UPDATE fry_hot_rewrite SET reserved_bytes=0 WHERE id=$1;

-- name: MemberServerServeReadMemberMaintenance :one
-- Used by serve in member_http_linux.go.
SELECT state,kind,owner_epoch FROM fry_member_maintenance WHERE id=$1 AND member=$2;

-- name: MemberServerServeCheckAttempt :one
-- Used by serve in member_http_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_attempt WHERE id=$1 AND organization=$2 AND receipt_id=$3);

-- name: OpenLocalMemberReadMember :one
-- Used by OpenLocalMember in member_linux.go.
SELECT incarnation,failure_domain,directory FROM fry_member WHERE id=$1;

-- name: OpenLocalMemberCheckMemberOwner :one
-- Used by OpenLocalMember in member_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member_owner WHERE member=$1 AND NOT closed);

-- name: OpenLocalMemberInsertMember :execresult
-- Used by OpenLocalMember in member_linux.go.
INSERT INTO fry_member(id,incarnation,failure_domain,directory) VALUES($1,$2,$3,$4);

-- name: OpenLocalMemberInsertMemberFile :execresult
-- Used by OpenLocalMember in member_linux.go.
INSERT INTO fry_member_file(member,file_id,directory,state,charged_bytes) VALUES($1,$2,$3,'current',$4);

-- name: LocalMemberCancelOwned :one
-- Used by cancelOwned in member_linux.go.
SELECT state FROM fry_attempt WHERE id=$1;

-- name: StoreAbort :many
-- Used by Abort in member_linux.go.
SELECT member FROM fry_target WHERE attempt=$1 ORDER BY member;

-- name: LocalMemberBeginMaintenanceKindReadMember :one
-- Used by beginMaintenanceKind in member_maintenance_linux.go.
SELECT incarnation,maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: LocalMemberBeginMaintenanceKindReadMemberMaintenance :one
-- Used by beginMaintenanceKind in member_maintenance_linux.go.
SELECT member,owner_epoch,expected_incarnation,state,kind FROM fry_member_maintenance WHERE id=$1;

-- name: LocalMemberBeginMaintenanceKindInsertMemberMaintenance :execresult
-- Used by beginMaintenanceKind in member_maintenance_linux.go.
INSERT INTO fry_member_maintenance(id,member,owner_epoch,expected_incarnation,state,kind) VALUES($1,$2,$3,$4,'running',$5);

-- name: LocalMemberBeginMaintenanceKindUpdateMember :execresult
-- Used by beginMaintenanceKind in member_maintenance_linux.go.
UPDATE fry_member SET maintenance_job=$2 WHERE id=$1;

-- name: LocalMemberFinishMaintenanceReadMember :one
-- Used by finishMaintenance in member_maintenance_linux.go.
SELECT maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: LocalMemberFinishMaintenanceUpdateMemberMaintenance :execresult
-- Used by finishMaintenance in member_maintenance_linux.go.
UPDATE fry_member_maintenance SET state=$2,completed_at=clock_timestamp() WHERE id=$1 AND state='running';

-- name: LocalMemberFinishMaintenanceUpdateMember :execresult
-- Used by finishMaintenance in member_maintenance_linux.go.
UPDATE fry_member SET maintenance_job=NULL WHERE id=$1;

-- name: LocalMemberRecoverMaintenanceReadMember :one
-- Used by recoverMaintenance in member_maintenance_linux.go.
SELECT maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: LocalMemberRecoverMaintenanceUpdateMemberMaintenance :execresult
-- Used by recoverMaintenance in member_maintenance_linux.go.
UPDATE fry_member_maintenance SET state='failed',completed_at=clock_timestamp() WHERE id=$1 AND state='running';

-- name: LocalMemberRecoverMaintenanceUpdateMember :execresult
-- Used by recoverMaintenance in member_maintenance_linux.go.
UPDATE fry_member SET maintenance_job=NULL WHERE id=$1;

-- name: OwnMemberReadAdvisoryLock :one
-- Used by ownMember in member_owner_linux.go.
SELECT pg_try_advisory_lock($1);

-- name: OwnMemberReadMember :one
-- Used by ownMember in member_owner_linux.go.
SELECT id FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: OwnMemberReadMemberOwner :one
-- Used by ownMember in member_owner_linux.go.
SELECT epoch,closed FROM fry_member_owner WHERE member=$1 ORDER BY epoch DESC LIMIT 1;

-- name: OwnMemberInsertMemberOwner :execresult
-- Used by ownMember in member_owner_linux.go.
INSERT INTO fry_member_owner(member,epoch,instance) VALUES($1,$2,$3);

-- name: MemberOwnerGuardReadMember :one
-- Used by guard in member_owner_linux.go.
SELECT id FROM fry_member WHERE id=$1 FOR SHARE;

-- name: MemberOwnerGuardReadMemberOwner :one
-- Used by guard in member_owner_linux.go.
SELECT epoch,instance,closed FROM fry_member_owner WHERE member=$1 ORDER BY epoch DESC LIMIT 1;

-- name: MemberOwnerClose :execresult
-- Used by Close in member_owner_linux.go.
UPDATE fry_member_owner SET closed=true WHERE member=$1 AND epoch=$2 AND instance=$3 AND NOT closed;

-- name: MemberOwnerReadReadReadPin :one
-- Used by read in member_read_linux.go.
SELECT extract(epoch FROM (deadline-clock_timestamp())) FROM fry_read_pin WHERE id=$1 AND receipt=$2 AND owner_member IS NULL AND deadline>clock_timestamp() FOR SHARE;

-- name: MemberOwnerReadInsertMemberRead :execresult
-- Used by read in member_read_linux.go.
INSERT INTO fry_member_read(id,member,owner_epoch,receipt,parent_pin,incarnation) VALUES($1,$2,$3,$4,$5,$6);

-- name: MemberOwnerReadInsertReadPin :execresult
-- Used by read in member_read_linux.go.
INSERT INTO fry_read_pin(id,receipt,deadline,owner_member,owner_epoch,incarnation) SELECT $1,$2,deadline,$3,$4,$5 FROM fry_read_pin p WHERE p.id=$6;

-- name: MemberOwnerReadUpdateMemberRead :execresult
-- Used by read in member_read_linux.go.
UPDATE fry_member_read SET closed=true WHERE id=$1 AND member=$2 AND owner_epoch=$3 AND NOT closed;

-- name: MemberOwnerReadDeleteReadPin :execresult
-- Used by read in member_read_linux.go.
DELETE FROM fry_read_pin WHERE id=$1 AND owner_member=$2 AND owner_epoch=$3;

-- name: ResolveMemberRead :one
-- Used by resolveMemberRead in member_read_source_linux.go.
SELECT t.locator,t.incarnation,r.attempt,f.directory,f.state,f.charged_bytes,f.sealed_records
 FROM fry_target t JOIN fry_receipt r ON r.attempt=t.attempt
 JOIN fry_member_file f ON f.member=t.member AND f.file_id=t.incarnation
 WHERE r.id=$1 AND r.organization=$2 AND t.member=$3 AND NOT t.bytes_removed
 AND f.state IN ('current','sealed') AND EXISTS(SELECT 1 FROM fry_read_pin p WHERE p.id=$4 AND p.receipt=r.id AND p.owner_member IS NULL AND p.deadline>clock_timestamp());

-- name: StoreReconcileMemberClosuresReadMemberRead :many
-- Used by ReconcileMemberClosures in member_reconcile_linux.go.
SELECT r.id FROM fry_member_read r JOIN fry_member_owner o ON o.member=r.member AND o.epoch=r.owner_epoch WHERE o.closed AND NOT r.closed ORDER BY r.id LIMIT $1 FOR UPDATE OF r SKIP LOCKED;

-- name: StoreReconcileMemberClosuresUpdateMemberRead :execresult
-- Used by ReconcileMemberClosures in member_reconcile_linux.go.
UPDATE fry_member_read SET closed=true WHERE id=ANY($1::text[]);

-- name: StoreReconcileMemberClosuresDeleteReadPin :execresult
-- Used by ReconcileMemberClosures in member_reconcile_linux.go.
DELETE FROM fry_read_pin p USING fry_member_read r WHERE p.id=r.id AND r.id=ANY($1::text[]) AND p.owner_member=r.member AND p.owner_epoch=r.owner_epoch;

-- name: StoreReconcileMemberClosuresReadTarget :many
-- Used by ReconcileMemberClosures in member_reconcile_linux.go.
SELECT t.attempt,t.member,t.owner_epoch FROM fry_target t JOIN fry_attempt a ON a.id=t.attempt JOIN fry_member_owner o ON o.member=t.member AND o.epoch=t.owner_epoch WHERE a.state='aborted' AND o.closed AND NOT t.cancel_confirmed ORDER BY t.attempt,t.member LIMIT $1;

-- name: StoreReconcileClosedTargetUpdateTarget :execresult
-- Used by reconcileClosedTarget in member_reconcile_linux.go.
UPDATE fry_target t SET cancel_confirmed=true WHERE t.attempt=$1 AND t.member=$2 AND t.owner_epoch=$3 AND NOT t.cancel_confirmed AND EXISTS(SELECT 1 FROM fry_member_owner o WHERE o.member=t.member AND o.epoch=t.owner_epoch AND o.closed);

-- name: StoreReconcileClosedTargetUpdateCleanup :execresult
-- Used by reconcileClosedTarget in member_reconcile_linux.go.
UPDATE fry_cleanup SET cancel_confirmed=true WHERE attempt=$1 AND member=$2;

-- name: StoreMemberStorageReadMember :one
-- Used by MemberStorage in member_storage_linux.go.
SELECT transaction_timestamp()::timestamptz,p.enabled,p.max_message_bytes,p.max_concurrent_appends,p.max_bytes,p.max_records,p.max_file_bytes,p.staging_bytes,p.lane_version,p.lane_count,m.incarnation,f.charged_bytes,o.epoch,o.closed FROM fry_member m JOIN fry_member_profile p ON p.member=m.id JOIN fry_member_file f ON f.member=m.id AND f.file_id=m.incarnation AND f.directory=m.directory AND f.state='current' LEFT JOIN LATERAL(SELECT epoch,closed FROM fry_member_owner WHERE member=m.id ORDER BY epoch DESC LIMIT 1)o ON true WHERE m.id=$1;

-- name: StoreMemberStorageReadMemberFileLane :many
-- Used by MemberStorage in member_storage_linux.go.
SELECT lane,record_ceiling,byte_ceiling,reserved_records,reserved_bytes FROM fry_member_file_lane WHERE member=$1 AND file_id=$2 ORDER BY lane LIMIT 17;

-- name: StoreMemberStorageReadMemberFile :many
-- Used by MemberStorage in member_storage_linux.go.
SELECT file_id,state,charged_bytes,sealed_records,rewrite_job FROM fry_member_file WHERE member<=$1 AND (member,file_id)>($1,$2) ORDER BY member,file_id LIMIT $3;

-- name: MigrationPassVanishedReadMigration :one
-- Used by Vanished in migration_finish_linux.go.
SELECT phase FROM fry_migration WHERE id=$1;

-- name: MigrationPassVanishedCheckMigrationFolder :one
-- Used by Vanished in migration_finish_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_folder WHERE fry_migration_folder.job=$1 AND source_name=$2 AND uidvalidity=$3 AND next_uid=$4 AND upper_uid>=$5);

-- name: MigrationPassVanishedUpdateMigrationEntry :execresult
-- Used by Vanished in migration_finish_linux.go.
UPDATE fry_migration_entry SET seen_generation=0,attempt=NULL WHERE job=$1 AND source_name=$2 AND uidvalidity=$3 AND uid=$4;

-- name: MigrationPassRequireScanned :one
-- Used by requireScanned in migration_finish_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_folder WHERE fry_migration_folder.job=$1 AND next_uid<=upper_uid) OR EXISTS(SELECT 1 FROM fry_migration_entry WHERE fry_migration_entry.job=$1 AND seen_generation=$2 AND mail IS NULL);

-- name: MigrationPassReconcileReadMigrationEntry :many
-- Used by Reconcile in migration_finish_linux.go.
SELECT e.id,m.id,m.receipt,m.folder FROM fry_migration_entry e JOIN fry_mail m ON m.id=e.mail WHERE e.job=$1 AND e.seen_generation<>$2 ORDER BY e.id LIMIT $3;

-- name: MigrationPassReconcileReadFolder :one
-- Used by Reconcile in migration_finish_linux.go.
SELECT id FROM fry_folder WHERE id=$1 FOR UPDATE;

-- name: MigrationPassReconcileUpdateMigrationEntry :execresult
-- Used by Reconcile in migration_finish_linux.go.
UPDATE fry_migration_entry SET mail=NULL WHERE id=$1 AND job=$2 AND seen_generation<>$3 AND mail=$4;

-- name: MigrationPassReconcileDeleteMail :execresult
-- Used by Reconcile in migration_finish_linux.go.
DELETE FROM fry_mail WHERE id=$1;

-- name: MigrationPassSealReadMigration :one
-- Used by Seal in migration_finish_linux.go.
SELECT inventory,phase FROM fry_migration WHERE id=$1;

-- name: MigrationPassSealCheckMigrationEntry :one
-- Used by Seal in migration_finish_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_entry e LEFT JOIN fry_mail m ON m.id=e.mail LEFT JOIN fry_migration_folder f ON f.job=e.job AND f.source_name=e.source_name AND f.uidvalidity=e.uidvalidity WHERE e.job=$1 AND ((e.seen_generation<>$2 AND e.mail IS NOT NULL) OR (e.seen_generation=$2 AND (m.id IS NULL OR f.folder IS NULL OR m.folder<>f.folder OR m.flags<>e.flags OR m.internal_date<>e.internal_date)))) OR EXISTS(SELECT 1 FROM fry_mail m JOIN fry_folder f ON f.id=m.folder WHERE f.inbox=$3 AND NOT EXISTS(SELECT 1 FROM fry_migration_entry e WHERE e.job=$1 AND e.mail=m.id AND e.seen_generation=$2));

-- name: MigrationPassSealDeleteFolder :execresult
-- Used by Seal in migration_finish_linux.go.
DELETE FROM fry_folder f WHERE inbox=$1 AND NOT EXISTS(SELECT 1 FROM fry_migration_folder mf WHERE mf.job=$2 AND mf.folder=f.id);

-- name: MigrationPassSealDeleteSubscription :execresult
-- Used by Seal in migration_finish_linux.go.
DELETE FROM fry_subscription WHERE inbox=$1;

-- name: MigrationPassSealInsertSubscription :execresult
-- Used by Seal in migration_finish_linux.go.
INSERT INTO fry_subscription(inbox,name) SELECT $1,destination FROM fry_migration_folder WHERE job=$2 AND subscribed;

-- name: MigrationPassSealReadMigrationEntry :many
-- Used by Seal in migration_finish_linux.go.
SELECT e.source_name,e.uidvalidity,e.uid,e.flags,e.internal_date,e.raw_size,m.uid,r.sha256 FROM fry_migration_entry e JOIN fry_mail m ON m.id=e.mail JOIN fry_receipt r ON r.id=m.receipt WHERE e.job=$1 AND e.seen_generation=$2 ORDER BY e.source_name,e.uidvalidity,e.uid;

-- name: MigrationPassSealUpdateMigration :execresult
-- Used by Seal in migration_finish_linux.go.
UPDATE fry_migration SET phase=$2,manifest=$3 WHERE id=$1;

-- name: MigrationPassSealInsertMigrationEvent :execresult
-- Used by Seal in migration_finish_linux.go.
INSERT INTO fry_migration_event(job,generation,kind,detail) VALUES($1,$2,'sealed',$3);

-- name: MigrationPassActivateReadMigration :one
-- Used by Activate in migration_finish_linux.go.
SELECT phase,generation FROM fry_migration WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: MigrationPassActivateUpdateInbox :execresult
-- Used by Activate in migration_finish_linux.go.
UPDATE fry_inbox SET migration_job=NULL WHERE id=$1 AND migration_job=$2;

-- name: MigrationPassActivateUpdateMigration :execresult
-- Used by Activate in migration_finish_linux.go.
UPDATE fry_migration SET phase='complete',completed_at=clock_timestamp() WHERE id=$1 AND manifest IS NOT NULL;

-- name: MigrationPassActivateInsertMigrationEvent :execresult
-- Used by Activate in migration_finish_linux.go.
INSERT INTO fry_migration_event(job,generation,kind,detail) VALUES($1,$2,'activated','{}');

-- name: MigrationPassActivateReadMigrationDeadlineClockTimestamp :one
-- Used by Activate in migration_finish_linux.go.
SELECT deadline>clock_timestamp() FROM fry_migration WHERE id=$1;

-- name: StoreReapMigrationAttempts :many
-- Used by ReapMigrationAttempts in migration_finish_linux.go.
SELECT c.attempt FROM fry_migration_copy c JOIN fry_migration j ON j.id=c.job JOIN fry_migration_entry e ON e.id=c.entry JOIN fry_attempt a ON a.id=c.attempt WHERE j.id=$1 AND j.organization=$2 AND c.receipt IS NULL AND a.state<>'accepted' AND (c.generation<>j.generation OR e.attempt IS DISTINCT FROM c.attempt OR j.phase='complete') AND (a.state<>'aborted' OR EXISTS(SELECT 1 FROM fry_target t WHERE t.attempt=a.id AND NOT t.cancel_confirmed)) ORDER BY c.attempt LIMIT $3;

-- name: StoreCreateMigrationReadMigration :one
-- Used by CreateMigration in migration_linux.go.
SELECT organization,inbox,fingerprint FROM fry_migration WHERE id=$1;

-- name: StoreCreateMigrationCheckDestination :one
-- Used by CreateMigration in migration_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_destination WHERE fry_destination.inbox=$1) OR EXISTS(SELECT 1 FROM fry_work WHERE fry_work.inbox=$1);

-- name: StoreCreateMigrationInsertInbox :execresult
-- Used by CreateMigration in migration_linux.go.
INSERT INTO fry_inbox(id,organization) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: StoreCreateMigrationInsertMigration :execresult
-- Used by CreateMigration in migration_linux.go.
INSERT INTO fry_migration(id,organization,inbox,fingerprint) VALUES($1,$2,$3,$4);

-- name: StoreCreateMigrationUpdateInbox :execresult
-- Used by CreateMigration in migration_linux.go.
UPDATE fry_inbox SET migration_job=$2 WHERE id=$1;

-- name: StoreCreateMigrationInsertMigrationEvent :execresult
-- Used by CreateMigration in migration_linux.go.
INSERT INTO fry_migration_event(job,generation,kind,detail) VALUES($1,0,'created',$2);

-- name: StoreBeginMigrationPassReadMigration :one
-- Used by BeginMigrationPass in migration_linux.go.
SELECT inbox,generation,phase,clock_timestamp()::timestamptz FROM fry_migration WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: StoreBeginMigrationPassDeleteMigrationFolder :execresult
-- Used by BeginMigrationPass in migration_linux.go.
DELETE FROM fry_migration_folder WHERE fry_migration_folder.job=$1;

-- name: StoreBeginMigrationPassInsertFolder :execresult
-- Used by BeginMigrationPass in migration_linux.go.
INSERT INTO fry_folder(id,inbox,name) VALUES($1,$2,$3) ON CONFLICT(inbox,name) DO NOTHING;

-- name: StoreBeginMigrationPassReadFolder :one
-- Used by BeginMigrationPass in migration_linux.go.
SELECT id FROM fry_folder WHERE inbox=$1 AND name=$2;

-- name: StoreBeginMigrationPassInsertMigrationFolder :execresult
-- Used by BeginMigrationPass in migration_linux.go.
INSERT INTO fry_migration_folder(job,source_name,destination,folder,uidvalidity,upper_uid,subscribed) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: StoreBeginMigrationPassUpdateMigration :execresult
-- Used by BeginMigrationPass in migration_linux.go.
UPDATE fry_migration SET generation=$2,phase=$3,freeze_attestation=$4,deadline=$5,inventory=$6,manifest=NULL WHERE id=$1;

-- name: StoreBeginMigrationPassInsertMigrationEvent :execresult
-- Used by BeginMigrationPass in migration_linux.go.
INSERT INTO fry_migration_event(job,generation,kind,detail) VALUES($1,$2,'pass-started',$3);

-- name: StoreResumeMigration :one
-- Used by ResumeMigration in migration_linux.go.
SELECT inbox,generation,phase,deadline FROM fry_migration WHERE id=$1 AND organization=$2;

-- name: MigrationPassGuard :one
-- Used by guard in migration_linux.go.
SELECT generation,phase,deadline IS NULL OR deadline>clock_timestamp() FROM fry_migration WHERE id=$1 AND organization=$2 AND inbox=$3 FOR UPDATE;

-- name: MigrationPassNextWindow :one
-- Used by NextWindow in migration_linux.go.
SELECT source_name,destination,uidvalidity,next_uid,upper_uid FROM fry_migration_folder WHERE fry_migration_folder.job=$1 AND next_uid<=upper_uid ORDER BY source_name LIMIT 1;

-- name: MigrationPassAdvanceWindowCheckMigrationEntry :one
-- Used by AdvanceWindow in migration_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_entry e JOIN fry_mail m ON m.id=e.mail WHERE e.job=$1 AND e.source_name=$2 AND e.uidvalidity=$3 AND e.uid=$4 AND e.seen_generation=$5);

-- name: MigrationPassAdvanceWindowReadMigration :one
-- Used by AdvanceWindow in migration_linux.go.
SELECT phase FROM fry_migration WHERE id=$1;

-- name: MigrationPassAdvanceWindowCheckMigrationEntryEXISTSSELECT1 :one
-- Used by AdvanceWindow in migration_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_entry WHERE fry_migration_entry.job=$1 AND source_name=$2 AND uidvalidity=$3 AND seen_generation=$4 AND uid BETWEEN $5 AND $6 AND NOT(uid=ANY($7::bigint[])));

-- name: MigrationPassAdvanceWindowUpdateMigrationEntry :execresult
-- Used by AdvanceWindow in migration_linux.go.
UPDATE fry_migration_entry SET seen_generation=0,attempt=NULL WHERE job=$1 AND source_name=$2 AND uidvalidity=$3 AND seen_generation=$4 AND uid BETWEEN $5 AND $6 AND NOT(uid=ANY($7::bigint[]));

-- name: MigrationPassAdvanceWindowUpdateMigrationFolder :execresult
-- Used by AdvanceWindow in migration_linux.go.
UPDATE fry_migration_folder SET next_uid=$5 WHERE job=$1 AND source_name=$2 AND uidvalidity=$3 AND next_uid=$4 AND upper_uid>=$6;

-- name: MigrationPassObserveReadMigrationEntry :one
-- Used by Observe in migration_linux.go.
SELECT m.receipt FROM fry_migration_entry e LEFT JOIN fry_mail m ON m.id=e.mail WHERE e.job=$1 AND e.source_name=$2 AND e.uidvalidity=$3 AND e.uid=$4;

-- name: MigrationPassObserveReadMigrationFolder :one
-- Used by Observe in migration_linux.go.
SELECT folder,next_uid,upper_uid FROM fry_migration_folder WHERE fry_migration_folder.job=$1 AND source_name=$2 AND uidvalidity=$3 AND destination=$4;

-- name: MigrationPassObserveInsertMigrationEntry :execresult
-- Used by Observe in migration_linux.go.
INSERT INTO fry_migration_entry(id,job,source_name,uidvalidity,uid,seen_generation,flags,internal_date,raw_size) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(job,source_name,uidvalidity,uid) DO NOTHING;

-- name: MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSize :one
-- Used by Observe in migration_linux.go.
SELECT id,mail,attempt,internal_date,raw_size FROM fry_migration_entry WHERE fry_migration_entry.job=$1 AND source_name=$2 AND uidvalidity=$3 AND uid=$4 FOR UPDATE;

-- name: MigrationPassObserveReadMail :one
-- Used by Observe in migration_linux.go.
SELECT receipt,folder FROM fry_mail WHERE id=$1;

-- name: MigrationPassObserveReadFolder :one
-- Used by Observe in migration_linux.go.
SELECT id FROM fry_folder WHERE id=$1 FOR UPDATE;

-- name: MigrationPassObserveUpdateMail :execresult
-- Used by Observe in migration_linux.go.
UPDATE fry_mail SET flags=$2 WHERE id=$1;

-- name: MigrationPassObserveReadMigrationCopy :one
-- Used by Observe in migration_linux.go.
SELECT c.envelope FROM fry_migration_copy c LEFT JOIN fry_attempt a ON a.id=c.attempt WHERE c.attempt=$1 AND c.generation=$2 AND (a.state IS NULL OR a.state<>'aborted');

-- name: MigrationPassObserveInsertMigrationCopy :execresult
-- Used by Observe in migration_linux.go.
INSERT INTO fry_migration_copy(attempt,job,entry,generation,envelope) VALUES($1,$2,$3,$4,$5);

-- name: MigrationPassObserveUpdateMigrationEntry :execresult
-- Used by Observe in migration_linux.go.
UPDATE fry_migration_entry SET attempt=$2 WHERE id=$1;

-- name: MigrationPassObserveUpdateMigrationEntry2 :execresult
-- Used by Observe in migration_linux.go.
UPDATE fry_migration_entry SET seen_generation=$2,flags=$3 WHERE id=$1;

-- name: GuardMigrationImportReadInbox :one
-- Used by guardMigrationImport in migration_linux.go.
SELECT migration_job FROM fry_inbox WHERE id=$1 FOR SHARE;

-- name: GuardMigrationImportCheckMigrationCopy :one
-- Used by guardMigrationImport in migration_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_migration_copy c JOIN fry_migration_entry e ON e.id=c.entry JOIN fry_inbox i ON i.id=$6 WHERE c.attempt=$1 AND c.job=$2 AND c.entry=$3 AND c.generation=$4 AND c.envelope=$5::jsonb AND c.receipt IS NULL AND e.attempt=c.attempt AND e.seen_generation=c.generation AND e.mail IS NULL AND i.migration_job=c.job);

-- name: FinishMigrationImportReadMigrationEntry :one
-- Used by finishMigrationImport in migration_linux.go.
SELECT raw_size,flags FROM fry_migration_entry WHERE id=$1;

-- name: FinishMigrationImportReadReceipt :one
-- Used by finishMigrationImport in migration_linux.go.
SELECT r.raw_size,m.id FROM fry_receipt r JOIN fry_mail m ON m.receipt=r.id WHERE r.id=$1;

-- name: FinishMigrationImportUpdateMail :execresult
-- Used by finishMigrationImport in migration_linux.go.
UPDATE fry_mail SET flags=$2 WHERE id=$1;

-- name: FinishMigrationImportUpdateMigrationEntry :execresult
-- Used by finishMigrationImport in migration_linux.go.
UPDATE fry_migration_entry SET mail=$2 WHERE id=$1;

-- name: FinishMigrationImportUpdateMigrationCopy :execresult
-- Used by finishMigrationImport in migration_linux.go.
UPDATE fry_migration_copy SET receipt=$2 WHERE attempt=$1;

-- name: MigrationPassInventory :one
-- Used by Inventory in migration_linux.go.
SELECT inventory FROM fry_migration WHERE id=$1 AND organization=$2 AND generation=$3;

-- name: LockInboxAdmission :execresult
-- Used by lockInboxAdmission in migration_linux.go.
SELECT pg_advisory_xact_lock(hashtextextended('fry-inbox-admission:' || $1,0));

-- name: LockOrdinaryInboxAdmission :execresult
-- Used by lockOrdinaryInboxAdmission in migration_linux.go.
SELECT pg_advisory_xact_lock_shared(hashtextextended('fry-inbox-admission:' || $1,0));

-- name: StoreMigrationProgress :one
-- Used by MigrationProgress in migration_progress_linux.go.
SELECT j.inbox,j.generation,j.phase,j.deadline,j.completed_at,j.manifest,(SELECT count(*) FROM fry_migration_entry e WHERE e.job=j.id AND e.mail IS NOT NULL),(SELECT count(*) FROM fry_migration_folder f WHERE f.job=j.id AND f.next_uid<=f.upper_uid),(SELECT count(*) FROM fry_migration_entry e WHERE e.job=j.id AND e.seen_generation=j.generation AND e.mail IS NULL) FROM fry_migration j WHERE j.id=$1 AND j.organization=$2;

-- name: LockInboxNamespace :one
-- Used by lockInboxNamespace in namespace_linux.go.
SELECT id FROM fry_inbox WHERE id=$1 AND organization=$2 AND migration_job IS NULL FOR UPDATE;

-- name: RecordNewFolderName :execresult
-- Used by recordNewFolderName in namespace_linux.go.
INSERT INTO fry_folder_name(inbox,name,folder,uidvalidity) SELECT inbox,name,id,uidvalidity FROM fry_folder WHERE id=$1 ON CONFLICT(inbox,name) DO UPDATE SET folder=excluded.folder,uidvalidity=excluded.uidvalidity;

-- name: StoreRenameFolderReadFolder :many
-- Used by RenameFolder in namespace_linux.go.
SELECT id,name,uidvalidity FROM fry_folder WHERE inbox=$1 ORDER BY id FOR UPDATE;

-- name: StoreRenameFolderReadFolderName :one
-- Used by RenameFolder in namespace_linux.go.
SELECT folder,uidvalidity FROM fry_folder_name WHERE inbox=$1 AND name=$2;

-- name: StoreRenameFolderReadSequence :one
-- Used by RenameFolder in namespace_linux.go.
SELECT nextval('fry_uidvalidity');

-- name: StoreRenameFolderUpdateFolder :execresult
-- Used by RenameFolder in namespace_linux.go.
UPDATE fry_folder SET name=$1,uidvalidity=$2 WHERE id=$3;

-- name: StoreListFoldersReadFolder :many
-- Used by ListFolders in namespace_linux.go.
SELECT name FROM fry_folder WHERE inbox=$1;

-- name: StoreListFoldersReadSubscription :many
-- Used by ListFolders in namespace_linux.go.
SELECT name FROM fry_subscription WHERE inbox=$1;

-- name: CreateFolderAncestors :execresult
-- Used by createFolderAncestors in namespace_linux.go.
INSERT INTO fry_folder(id,inbox,name) VALUES($1,$2,$3);

-- name: StoreRenameInboxReadFolder :many
-- Used by renameInbox in namespace_linux.go.
SELECT id,name FROM fry_folder WHERE inbox=$1 ORDER BY id FOR UPDATE;

-- name: StoreRenameInboxInsertFolder :execresult
-- Used by renameInbox in namespace_linux.go.
INSERT INTO fry_folder(id,inbox,name) VALUES($1,$2,$3);

-- name: StoreQueueFailureNoticesReadFailureNotice :many
-- Used by QueueFailureNotices in notices_linux.go.
SELECT id FROM fry_failure_notice WHERE state='pending' ORDER BY created_at,id LIMIT $1;

-- name: StoreQueueFailureNoticesUpdateFailureNotice :execresult
-- Used by QueueFailureNotices in notices_linux.go.
UPDATE fry_failure_notice SET state='queued',receipt=$2 WHERE id=$1;

-- name: StorePrepareNoticeReadFailureNotice :one
-- Used by prepareNotice in notices_linux.go.
SELECT organization,relay,recipient,failed_recipient,response_code,diagnostic,created_at,rendering_host,notice_retry_seconds FROM fry_failure_notice WHERE id=$1 FOR UPDATE;

-- name: StorePrepareNoticeUpdateFailureNotice :execresult
-- Used by prepareNotice in notices_linux.go.
UPDATE fry_failure_notice SET rendering_host=$2,notice_retry_seconds=$3 WHERE id=$1;

-- name: StoreBindObjectNamespaceReadObjectNamespace :one
-- Used by bindObjectNamespace in object_namespace_linux.go.
SELECT namespace FROM fry_object_namespace WHERE singleton FOR UPDATE;

-- name: StoreBindObjectNamespaceCheckPack :one
-- Used by bindObjectNamespace in object_namespace_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_pack);

-- name: StoreBindObjectNamespaceUpdateObjectNamespace :execresult
-- Used by bindObjectNamespace in object_namespace_linux.go.
UPDATE fry_object_namespace SET namespace=$1 WHERE singleton;

-- name: PublishOutboundInsertOutbound :execresult
-- Used by publishOutbound in outbound_linux.go.
INSERT INTO fry_outbound(id,receipt,relay,recipient,deadline) VALUES($1,$2,$3,$4,$5);

-- name: PublishOutboundInsertReference :execresult
-- Used by publishOutbound in outbound_linux.go.
INSERT INTO fry_reference(receipt,kind,reference_key) VALUES($1,'delivery',$2);

-- name: StoreClaimOutboundReadOutbound :one
-- Used by ClaimOutbound in outbound_linux.go.
SELECT q.id,q.receipt,r.organization,q.relay,q.recipient,r.raw_size,q.deadline,a.envelope FROM fry_outbound q JOIN fry_receipt r ON r.id=q.receipt JOIN fry_attempt a ON a.id=r.attempt WHERE q.state='queued' AND q.available_at<=clock_timestamp() AND (q.lease_until IS NULL OR q.lease_until<=clock_timestamp()) ORDER BY q.available_at,q.id LIMIT 1 FOR UPDATE OF q SKIP LOCKED;

-- name: StoreClaimOutboundUpdateOutbound :one
-- Used by ClaimOutbound in outbound_linux.go.
UPDATE fry_outbound SET epoch=epoch+1,attempts=attempts+1,lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond' WHERE id=$1 RETURNING epoch,attempts,lease_until;

-- name: StoreCompleteOutboundReadOutbound :one
-- Used by CompleteOutbound in outbound_linux.go.
SELECT state,receipt,relay,recipient,epoch,lease_until,deadline,clock_timestamp()::timestamptz FROM fry_outbound WHERE id=$1 FOR UPDATE;

-- name: StoreCompleteOutboundUpdateOutbound :execresult
-- Used by CompleteOutbound in outbound_linux.go.
UPDATE fry_outbound SET available_at=LEAST(clock_timestamp()+$2::bigint*interval '1 microsecond',deadline),lease_until=NULL,response_code=$3,diagnostic=$4 WHERE id=$1;

-- name: StoreCompleteOutboundUpdateOutbound2 :execresult
-- Used by CompleteOutbound in outbound_linux.go.
UPDATE fry_outbound SET state=$2,completed_at=clock_timestamp(),lease_until=NULL,response_code=$3,diagnostic=$4 WHERE id=$1;

-- name: StoreCompleteOutboundReadReceipt :one
-- Used by CompleteOutbound in outbound_linux.go.
SELECT (a.envelope->>'MailFrom')::text FROM fry_receipt r JOIN fry_attempt a ON a.id=r.attempt WHERE r.id=$1;

-- name: StoreCompleteOutboundInsertFailureNotice :execresult
-- Used by CompleteOutbound in outbound_linux.go.
INSERT INTO fry_failure_notice(id,job,organization,relay,recipient,failed_recipient,response_code,diagnostic) VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: StoreOutboundState :many
-- Used by OutboundState in outbound_linux.go.
SELECT q.recipient,q.state FROM fry_outbound q JOIN fry_receipt r ON r.id=q.receipt WHERE r.organization=$1 AND r.id=$2;

-- name: PackerBuild :execresult
-- Used by build in packing_linux.go.
UPDATE fry_pack SET staging_closed=true WHERE id=$1;

-- name: PackerRegisterReadReference :one
-- Used by register in packing_linux.go.
SELECT r.manifest_version,(r.pack_id IS NULL)::boolean,a.recovery_seconds,EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())) FROM fry_receipt r JOIN fry_attempt a ON a.receipt_id=r.id WHERE r.id=$1 AND r.organization=$2 FOR UPDATE OF r;

-- name: PackerRegisterCheckReference :one
-- Used by register in packing_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_reference WHERE fry_reference.receipt=$1 AND (expires_at IS NULL OR expires_at>clock_timestamp()));

-- name: PackerRegisterInsertPack :execresult
-- Used by register in packing_linux.go.
INSERT INTO fry_pack(id,organization,object_key,state,lease_until,staging_owner) VALUES($1,$2,$3,'building',clock_timestamp()+$4::bigint*interval '1 microsecond',$5);

-- name: PackerRegisterInsertPackMember :execresult
-- Used by register in packing_linux.go.
INSERT INTO fry_pack_member(pack,receipt,manifest_version) VALUES($1,$2,$3);

-- name: PackGuardReadPack :one
-- Used by packGuard in packing_linux.go.
SELECT state,epoch,lease_until FROM fry_pack WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: PackGuardReadClock :one
-- Used by packGuard in packing_linux.go.
SELECT clock_timestamp()::timestamptz;

-- name: PackerRecordImageUpdatePack :execresult
-- Used by recordImage in packing_linux.go.
UPDATE fry_pack SET image_size=$2,image_sha256=$3 WHERE id=$1;

-- name: PackerRecordImageUpdatePackMember :execresult
-- Used by recordImage in packing_linux.go.
UPDATE fry_pack_member SET locator=$3 WHERE pack=$1 AND receipt=$2;

-- name: PackerPublishReadReference :one
-- Used by publish in packing_linux.go.
SELECT manifest_version,(pack_id IS NULL)::boolean,EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())) FROM fry_receipt r WHERE r.id=$1 AND r.organization=$2 FOR UPDATE;

-- name: PackerPublishReadPack :one
-- Used by publish in packing_linux.go.
SELECT image_size,image_sha256 FROM fry_pack WHERE id=$1;

-- name: PackerPublishUpdateReceipt :execresult
-- Used by publish in packing_linux.go.
UPDATE fry_receipt SET pack_id=$2,manifest_version=manifest_version+1 WHERE id=$1;

-- name: PackerPublishUpdatePack :execresult
-- Used by publish in packing_linux.go.
UPDATE fry_pack SET state='published',etag=$2 WHERE id=$1;

-- name: PackerStatus :one
-- Used by Status in packing_linux.go.
SELECT state FROM fry_pack WHERE id=$1 AND organization=$2;

-- name: PackerRetireReadPack :one
-- Used by Retire in packing_linux.go.
SELECT state,object_key FROM fry_pack WHERE id=$1 AND organization=$2 FOR UPDATE;

-- name: PackerRetireReadReadPin :one
-- Used by Retire in packing_linux.go.
SELECT count(*) FROM fry_read_pin WHERE pack=$1;

-- name: PackerRetireUpdatePack :execresult
-- Used by Retire in packing_linux.go.
UPDATE fry_pack SET state='retiring',epoch=epoch+1 WHERE id=$1;

-- name: PackerRetireUpdatePackRetired :execresult
-- Used by Retire in packing_linux.go.
UPDATE fry_pack SET state='retired' WHERE id=$1 AND state='retiring';

-- name: LivePackMembers :one
-- Used by livePackMembers in packing_linux.go.
SELECT count(DISTINCT m.receipt) FROM fry_pack_member m JOIN fry_reference f ON f.receipt=m.receipt WHERE m.pack=$1 AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp());

-- name: ProfileLock :one
-- Used by profileLock in postgres.go.
SELECT epoch FROM fry_profile WHERE singleton FOR SHARE;

-- name: AttemptLock :one
-- Used by attemptLock in postgres.go.
SELECT receipt_id,organization,envelope,state,epoch,profile_epoch,lease_until,recovery_seconds,ingest_shard,placement_version FROM fry_attempt WHERE id=$1 FOR UPDATE;

-- name: Live :one
-- Used by live in postgres.go.
SELECT clock_timestamp()::timestamptz;

-- name: AuthorityPrepareMembersTxInsertAttempt :execresult
-- Used by prepareMembersTx in postgres.go.
INSERT INTO fry_attempt(id,receipt_id,organization,envelope,state,epoch,profile_epoch,lease_until,recovery_seconds,cleanup_microseconds,placement_version,ingest_shard)
 VALUES($1,$2,$3,$4,'preparing',1,$5,clock_timestamp()+$6::bigint*interval '1 microsecond',$7,$8,1,$9) ON CONFLICT(id) DO NOTHING;

-- name: AuthorityPrepareMembersTxCheckIngestCatalog :one
-- Used by prepareMembersTx in postgres.go.
SELECT EXISTS(SELECT 1 FROM fry_ingest_catalog WHERE singleton AND version=1 AND algorithm='attempt-sha256-v1' AND catalog=$4::jsonb) AND EXISTS(SELECT 1 FROM fry_ingest_shard WHERE id=$1 AND member_a=$2 AND member_b=$3);

-- name: AuthorityPrepareMembersTxReadClock :one
-- Used by prepareMembersTx in postgres.go.
SELECT clock_timestamp()::timestamptz;

-- name: AuthorityPrepareMembersTxUpdateAttempt :execresult
-- Used by prepareMembersTx in postgres.go.
UPDATE fry_attempt SET epoch=$2,lease_until=clock_timestamp()+$3::bigint*interval '1 microsecond' WHERE id=$1;

-- name: AuthorityPrepareMembersTxInsertTarget :execresult
-- Used by prepareMembersTx in postgres.go.
INSERT INTO fry_target(attempt,member,incarnation,failure_domain,owner_epoch) VALUES($1,$2,$3,$4,$5);

-- name: AuthorityPrepareMembersTxInsertSource :execresult
-- Used by prepareMembersTx in postgres.go.
INSERT INTO fry_source(connection,folder,uidvalidity,uid,attempt) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING;

-- name: AuthorityPrepareMembersTxCheckTarget :one
-- Used by prepareMembersTx in postgres.go.
SELECT EXISTS(SELECT 1 FROM fry_target WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND failure_domain=$4 AND owner_epoch=$5);

-- name: AuthorityPrepareMembersTxReadAttempt :one
-- Used by prepareMembersTx in postgres.go.
SELECT lease_until>clock_timestamp() FROM fry_attempt WHERE id=$1;

-- name: AuthorityValidateTarget :one
-- Used by validateTarget in postgres.go.
SELECT a.state,a.epoch,a.profile_epoch,p.epoch,extract(epoch FROM (a.lease_until-clock_timestamp()))
 FROM fry_attempt a JOIN fry_target t ON t.attempt=a.id CROSS JOIN fry_profile p
 WHERE a.id=$1 AND t.member=$2 AND t.incarnation=$3 AND a.receipt_id=$4 AND a.organization=$5 AND t.owner_epoch=$6
 AND EXISTS(SELECT 1 FROM fry_member m JOIN fry_member_profile p ON p.member=m.id JOIN fry_member_file f ON f.member=m.id AND f.file_id=m.incarnation AND f.directory=m.directory AND f.state='current' AND f.rewrite_job IS NULL WHERE m.id=t.member AND m.incarnation=t.incarnation AND (NOT p.enabled OR (t.capacity_reserved_bytes IS NOT NULL AND t.capacity_lane IS NOT NULL AND EXISTS(SELECT 1 FROM fry_member_file_lane l WHERE l.member=t.member AND l.file_id=t.incarnation AND l.lane=t.capacity_lane AND l.reserved_records>0 AND l.reserved_bytes>=t.capacity_reserved_bytes))));

-- name: AuthorityRecordCopy :one
-- Used by recordCopy in postgres.go.
UPDATE fry_target SET locator=$4 WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND NOT cancel_confirmed AND owner_epoch=$5 RETURNING capacity_reserved_bytes,capacity_settled,capacity_lane;

-- name: AuthorityRenew :execresult
-- Used by renew in postgres.go.
UPDATE fry_attempt SET lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond' WHERE id=$1;

-- name: AuthorityStatus :one
-- Used by status in postgres.go.
SELECT state,epoch FROM fry_attempt WHERE id=$1 AND organization=$2;

-- name: AuthorityFinalizeWithHeaderReadTarget :many
-- Used by finalizeWithHeader in postgres.go.
SELECT member,incarnation,failure_domain,locator,cancel_confirmed FROM fry_target WHERE attempt=$1 ORDER BY member;

-- name: AuthorityFinalizeWithHeaderInsertReceipt :one
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_receipt(id,attempt,organization,received_at,raw_size,sha256,profile_epoch,protection) VALUES($1,$2,$3,clock_timestamp(),$4,$5,$6,'two-domain') RETURNING received_at;

-- name: AuthorityFinalizeWithHeaderInsertHeaderMetadata :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_header_metadata(receipt,raw_header) VALUES($1,$2);

-- name: AuthorityFinalizeWithHeaderInsertDestination :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_destination(receipt,inbox,recipients) VALUES($1,$2,$3);

-- name: AuthorityFinalizeWithHeaderInsertReference :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_reference(receipt,kind,reference_key,expires_at) VALUES($1,'pending-mailbox',$2,NULL);

-- name: AuthorityFinalizeWithHeaderInsertWork :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_work(receipt,inbox,kind,folder,flags,internal_date,organization,received_at) SELECT id,$2,'publish-mailbox',$3,$4,$5,organization,received_at FROM fry_receipt WHERE id=$1;

-- name: AuthorityFinalizeWithHeaderInsertAppendResult :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_append_result(receipt,folder,uidvalidity,uid) SELECT m.receipt,m.folder,f.uidvalidity,m.uid FROM fry_mail m JOIN fry_folder f ON f.id=m.folder WHERE m.receipt=$1 AND m.folder=$2;

-- name: AuthorityFinalizeWithHeaderInsertReferenceRecovery :execresult
-- Used by finalizeWithHeader in postgres.go.
INSERT INTO fry_reference(receipt,kind,reference_key,expires_at) VALUES($1,'recovery','original',$2::timestamptz+$3::bigint*interval '1 second');

-- name: AuthorityFinalizeWithHeaderUpdateSource :execresult
-- Used by finalizeWithHeader in postgres.go.
UPDATE fry_source SET accepted=true WHERE attempt=$1;

-- name: AuthorityFinalizeWithHeaderUpdateAttempt :execresult
-- Used by finalizeWithHeader in postgres.go.
UPDATE fry_attempt SET state='accepted' WHERE id=$1;

-- name: AuthorityAbortUpdateAttempt :execresult
-- Used by abort in postgres.go.
UPDATE fry_attempt SET state='aborted' WHERE id=$1;

-- name: AuthorityAbortDeleteSource :execresult
-- Used by abort in postgres.go.
DELETE FROM fry_source WHERE attempt=$1 AND NOT accepted;

-- name: AuthorityAbortInsertCleanup :execresult
-- Used by abort in postgres.go.
INSERT INTO fry_cleanup(attempt,member) SELECT t.attempt,t.member FROM fry_target t WHERE t.attempt=$1 ON CONFLICT DO NOTHING;

-- name: AuthorityConfirmCancellationUpdateTarget :execresult
-- Used by confirmCancellation in postgres.go.
UPDATE fry_target SET cancel_confirmed=true WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND owner_epoch=$4;

-- name: AuthorityConfirmCancellationUpdateCleanup :execresult
-- Used by confirmCancellation in postgres.go.
UPDATE fry_cleanup SET cancel_confirmed=true WHERE attempt=$1 AND member=$2;

-- name: GuardMemberOwnerReadMember :one
-- Used by guardMemberOwner in postgres.go.
SELECT id FROM fry_member WHERE id=$1 FOR SHARE;

-- name: GuardMemberOwnerCheckMemberOwner :one
-- Used by guardMemberOwner in postgres.go.
SELECT EXISTS(SELECT 1 FROM fry_member_owner WHERE member=$1 AND NOT closed);

-- name: StoreBindRelayDestinationsReadRelayBinding :one
-- Used by BindRelayDestinations in relay_binding_linux.go.
SELECT address,transport,username FROM fry_relay_binding WHERE id=$1;

-- name: StoreBindRelayDestinationsCheckOutbound :one
-- Used by BindRelayDestinations in relay_binding_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_outbound WHERE fry_outbound.relay=$1 UNION ALL SELECT 1 FROM fry_failure_notice WHERE fry_failure_notice.relay=$1);

-- name: StoreBindRelayDestinationsInsertRelayBinding :execresult
-- Used by BindRelayDestinations in relay_binding_linux.go.
INSERT INTO fry_relay_binding(id,address,transport,username) VALUES($1,$2,$3,$4);

-- name: StoreBindRelayDestinationsCheckOutboundQueued :one
-- Used by BindRelayDestinations in relay_binding_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_outbound WHERE state='queued' AND NOT(relay=ANY($1::text[])) UNION ALL SELECT 1 FROM fry_failure_notice WHERE state='pending' AND NOT(relay=ANY($1::text[])));

-- name: StoreHasPendingOutbound :one
-- Used by HasPendingOutbound in relay_binding_linux.go.
SELECT
 EXISTS(SELECT 1 FROM fry_outbound WHERE state='queued') OR
 EXISTS(SELECT 1 FROM fry_failure_notice WHERE state='pending') OR
 EXISTS(SELECT 1 FROM fry_attempt WHERE state='preparing' AND lease_until>clock_timestamp() AND jsonb_typeof(envelope->'Outbound')='object');

-- name: RemoteMemberCancelAttempt :one
-- Used by cancelAttempt in remote_linux.go.
SELECT organization,receipt_id FROM fry_attempt WHERE id=$1;

-- name: RotationCapacityReadMemberProfile :one
-- Used by rotationCapacity in rotation_linux.go.
SELECT p.enabled,p.max_file_bytes,p.staging_bytes,p.max_bytes,f.charged_bytes FROM fry_member_profile p JOIN fry_member_file f ON f.member=p.member WHERE p.member=$1 AND f.file_id=$2 AND f.state='current' FOR UPDATE OF p;

-- name: RotationCapacityCheckMemberFile :one
-- Used by rotationCapacity in rotation_linux.go.
SELECT (COALESCE((SELECT sum(charged_bytes) FROM fry_member_file WHERE fry_member_file.member=$1 AND state<>'removed'),0)+COALESCE((SELECT sum(reserved_bytes) FROM fry_hot_rewrite WHERE fry_hot_rewrite.member=$1 AND state='building'),0))::bigint;

-- name: StoreRotateHotInsertHotRewrite :execresult
-- Used by rotateHot in rotation_linux.go.
INSERT INTO fry_hot_rewrite(id,member,old_incarnation,old_directory,new_directory,kind,state,reserved_bytes) VALUES($1,$2,$3,$4,$5,'rotate','building',$6);

-- name: StoreRotateHotReadMember :one
-- Used by rotateHot in rotation_linux.go.
SELECT incarnation,maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: StoreRotateHotUpdateMemberFile :execresult
-- Used by rotateHot in rotation_linux.go.
UPDATE fry_member_file SET state='sealed',rewrite_job=$3,charged_bytes=$4,sealed_records=$5 WHERE member=$1 AND file_id=$2 AND directory=$6 AND state='current' AND rewrite_job IS NULL;

-- name: StoreRotateHotUpdateMemberFileCurrent :execresult
-- Used by rotateHot in rotation_linux.go.
UPDATE fry_member_file SET state='current',rewrite_job=NULL WHERE member=$1 AND file_id=$2 AND directory=$4 AND state='building' AND rewrite_job=$3;

-- name: StoreRotateHotUpdateMember :execresult
-- Used by rotateHot in rotation_linux.go.
UPDATE fry_member SET incarnation=$2,directory=$3 WHERE id=$1;

-- name: StoreRotateHotUpdateHotRewrite :execresult
-- Used by rotateHot in rotation_linux.go.
UPDATE fry_hot_rewrite SET state='published' WHERE id=$1 AND member=$2 AND kind='rotate' AND state='building' AND new_incarnation=$3 AND reserved_bytes=0;

-- name: PreflightSealedFiles :many
-- Used by preflightSealedFiles in rotation_linux.go.
SELECT f.file_id,f.directory,f.charged_bytes,f.sealed_records,p.max_records,p.max_file_bytes,p.enabled,
 EXISTS(SELECT 1 FROM fry_hot_rewrite j JOIN fry_member_file n ON n.member=j.member AND n.file_id=j.new_incarnation AND n.directory=j.new_directory WHERE j.id=f.rewrite_job AND j.member=f.member AND ((j.kind='rotate' AND j.old_incarnation=f.file_id AND j.old_directory=f.directory AND j.state IN ('published','complete')) OR (j.kind='sealed-reclaim' AND j.new_incarnation=f.file_id AND j.new_directory=f.directory AND j.kept_records=f.sealed_records AND j.state IN ('published','removed'))) AND j.new_incarnation<>j.old_incarnation AND j.new_directory<>j.old_directory)
 FROM fry_member_file f JOIN fry_member_profile p ON p.member=f.member WHERE f.member=$1 AND f.state='sealed' ORDER BY f.file_id;

-- name: LocalMemberSelectSealedReadMemberMaintenance :one
-- Used by selectSealed in sealed_reclaim_linux.go.
SELECT selected_file FROM fry_member_maintenance WHERE id=$1 AND member=$2 AND kind='sealed-reclaim' AND state='running' FOR UPDATE;

-- name: LocalMemberSelectSealedReadMemberFile :one
-- Used by selectSealed in sealed_reclaim_linux.go.
SELECT file_id,directory,charged_bytes,sealed_records FROM ((SELECT file_id,directory,charged_bytes,sealed_records FROM fry_member_file WHERE fry_member_file.member=$1 AND state='sealed' AND fry_member_file.file_id>$2 ORDER BY fry_member_file.file_id LIMIT 1) UNION ALL (SELECT file_id,directory,charged_bytes,sealed_records FROM fry_member_file WHERE fry_member_file.member=$1 AND state='sealed' AND fry_member_file.file_id<=$2 ORDER BY fry_member_file.file_id LIMIT 1)) candidates ORDER BY (file_id>$2) DESC LIMIT 1;

-- name: LocalMemberSelectSealedUpdateMemberMaintenance :execresult
-- Used by selectSealed in sealed_reclaim_linux.go.
UPDATE fry_member_maintenance SET selected_file=$3 WHERE id=$1 AND member=$2 AND selected_file IS NULL AND state='running';

-- name: LocalMemberSelectSealedReadMemberFileSealed :one
-- Used by selectSealed in sealed_reclaim_linux.go.
SELECT file_id,directory,charged_bytes,sealed_records FROM fry_member_file WHERE member=$1 AND file_id=$2 AND state='sealed';

-- name: StoreReclaimSealedFileInsertHotRewrite :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
INSERT INTO fry_hot_rewrite(id,member,old_incarnation,old_directory,new_directory,kind,state,reserved_bytes,kept_records) VALUES($1,$2,$3,$4,$5,'sealed-reclaim','building',$6,$7);

-- name: StoreReclaimSealedFileReadMember :one
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
SELECT incarnation,maintenance_job FROM fry_member WHERE id=$1 FOR UPDATE;

-- name: StoreReclaimSealedFileUpdateTarget :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
UPDATE fry_target SET incarnation=$3,locator=$4 WHERE attempt=$1 AND member=$2 AND incarnation=$5 AND NOT bytes_removed;

-- name: StoreReclaimSealedFileUpdateReceipt :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
UPDATE fry_receipt SET manifest_version=manifest_version+1 WHERE id=$1;

-- name: StoreReclaimSealedFileUpdateMemberFile :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
UPDATE fry_member_file SET state='sealed',charged_bytes=$4,sealed_records=$5 WHERE member=$1 AND file_id=$2 AND state='building' AND rewrite_job=$3;

-- name: StoreReclaimSealedFileUpdateMemberFileRetiring :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
UPDATE fry_member_file SET state='retiring',rewrite_job=$3 WHERE member=$1 AND file_id=$2 AND state='sealed' AND directory=$4 AND charged_bytes=$5 AND sealed_records=$6;

-- name: StoreReclaimSealedFileUpdateHotRewrite :execresult
-- Used by reclaimSealedFile in sealed_reclaim_linux.go.
UPDATE fry_hot_rewrite SET state='published' WHERE id=$1 AND member=$2 AND kind='sealed-reclaim' AND state='building' AND kept_records=$3 AND reserved_bytes=0;

-- name: LoadSealedReclaimJob :one
-- Used by loadSealedReclaimJob in sealed_reclaim_linux.go.
SELECT id,member,old_incarnation,old_directory,new_directory,state,new_incarnation,kept_records,reserved_bytes FROM fry_hot_rewrite WHERE id=$1 AND kind='sealed-reclaim';

-- name: PreflightSealedReclaimJobCheckMemberFile :one
-- Used by preflightSealedReclaimJob in sealed_reclaim_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_member_file WHERE member=$1 AND file_id=$2 AND directory=$3 AND state='sealed');

-- name: PreflightSealedReclaimJobReadMemberFile :one
-- Used by preflightSealedReclaimJob in sealed_reclaim_linux.go.
SELECT sealed_records FROM fry_member_file WHERE member=$1 AND file_id=$2;

-- name: LocalMemberReconcileSealedReclaimUpdateMemberFileLane :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_member_file_lane SET reserved_records=0,reserved_bytes=0 WHERE member=$1 AND file_id=$2;

-- name: LocalMemberReconcileSealedReclaimUpdateMemberFile :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_member_file SET state='removed',charged_bytes=0 WHERE member=$1 AND file_id=$2 AND state='building' AND rewrite_job=$3;

-- name: LocalMemberReconcileSealedReclaimUpdateHotRewrite :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_hot_rewrite SET state='abandoned',reserved_bytes=0 WHERE id=$1 AND state='building' AND kind='sealed-reclaim';

-- name: LocalMemberReconcileSealedReclaimUpdateTarget :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_target SET bytes_removed=true WHERE member=$1 AND incarnation=$2;

-- name: LocalMemberReconcileSealedReclaimUpdateCleanup :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_cleanup c SET bytes_removed=true FROM fry_target t WHERE c.attempt=t.attempt AND c.member=t.member AND t.member=$1 AND t.incarnation=$2 AND t.bytes_removed;

-- name: LocalMemberReconcileSealedReclaimUpdateMemberFileLane2 :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_member_file_lane SET reserved_records=0,reserved_bytes=0 WHERE member=$1 AND file_id=$2;

-- name: LocalMemberReconcileSealedReclaimUpdateMemberFileRemoved :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_member_file SET state='removed',charged_bytes=0 WHERE member=$1 AND file_id=$2 AND state='retiring' AND rewrite_job=$3;

-- name: LocalMemberReconcileSealedReclaimUpdateHotRewriteRemoved :execresult
-- Used by reconcileSealedReclaim in sealed_reclaim_linux.go.
UPDATE fry_hot_rewrite SET state='removed' WHERE id=$1 AND state='published' AND kind='sealed-reclaim';

-- name: EnsureSenderGrantInsertSenderGrant :execresult
-- Used by ensureSenderGrant in sender_authority_linux.go.
INSERT INTO fry_sender_grant(id,organization,scope) VALUES($1,$2,$3) ON CONFLICT(organization,scope) DO NOTHING;

-- name: EnsureSenderGrantReadSenderGrant :one
-- Used by ensureSenderGrant in sender_authority_linux.go.
SELECT id,challenge_epoch FROM fry_sender_grant WHERE organization=$1 AND scope=$2 FOR UPDATE;

-- name: InvalidateSenderChallenges :execresult
-- Used by invalidateSenderChallenges in sender_authority_linux.go.
UPDATE fry_sender_challenge SET state='superseded' WHERE grant_id=$1 AND state='pending';

-- name: CreateSenderChallengeUpdateSenderGrant :execresult
-- Used by CreateSenderChallenge in sender_authority_linux.go.
UPDATE fry_sender_grant SET challenge_epoch=challenge_epoch+1 WHERE id=$1;

-- name: CreateSenderChallengeInsertSenderChallenge :one
-- Used by CreateSenderChallenge in sender_authority_linux.go.
INSERT INTO fry_sender_challenge(id,grant_id,epoch,token_hash,expires_at) VALUES($1,$2,$3,$4,clock_timestamp()+interval '24 hours') RETURNING expires_at;

-- name: LookupSenderChallenge :one
-- Used by LookupSenderChallenge in sender_authority_linux.go.
SELECT g.scope,c.expires_at FROM fry_sender_challenge c JOIN fry_sender_grant g ON g.id=c.grant_id WHERE c.id=$1 AND g.organization=$2 AND c.epoch=g.challenge_epoch AND c.state='pending' AND c.expires_at>clock_timestamp();

-- name: CompleteSenderChallengeReadSenderChallenge :one
-- Used by CompleteSenderChallenge in sender_authority_linux.go.
SELECT g.id FROM fry_sender_challenge c JOIN fry_sender_grant g ON g.id=c.grant_id WHERE c.id=$1 AND g.organization=$2 FOR UPDATE OF g;

-- name: CompleteSenderChallengeReadSenderChallengePending :one
-- Used by CompleteSenderChallenge in sender_authority_linux.go.
SELECT c.token_hash FROM fry_sender_challenge c JOIN fry_sender_grant g ON g.id=c.grant_id WHERE c.id=$1 AND c.grant_id=$2 AND c.epoch=g.challenge_epoch AND c.state='pending' AND c.expires_at>clock_timestamp() FOR UPDATE OF c;

-- name: CompleteSenderChallengeUpdateSenderChallenge :execresult
-- Used by CompleteSenderChallenge in sender_authority_linux.go.
UPDATE fry_sender_challenge SET state='completed',completed_by=$2 WHERE id=$1;

-- name: ActivateSenderGrant :execresult
-- Used by activateSenderGrant in sender_authority_linux.go.
UPDATE fry_sender_grant SET active=true,version=version+1,challenge_epoch=challenge_epoch+1,actor=$2,evidence=$3,reason=$4,verified_at=clock_timestamp() WHERE id=$1;

-- name: SenderGrantEvent :execresult
-- Used by senderGrantEvent in sender_authority_linux.go.
INSERT INTO fry_sender_grant_event(grant_id,version,action,actor,evidence,reason) SELECT id,version,$2,$3,$4,$5 FROM fry_sender_grant WHERE id=$1;

-- name: RevokeSenderGrant :execresult
-- Used by RevokeSenderGrant in sender_authority_linux.go.
UPDATE fry_sender_grant SET active=false,version=version+1,challenge_epoch=challenge_epoch+1,actor=$2,reason=$3 WHERE id=$1;

-- name: GetSenderGrant :one
-- Used by getSenderGrant in sender_authority_linux.go.
SELECT id,organization,scope,version,active,evidence,actor,reason,verified_at FROM fry_sender_grant WHERE id=$1;

-- name: ListSenderGrants :many
-- Used by ListSenderGrants in sender_authority_linux.go.
SELECT id,organization,scope,version,active,evidence,actor,reason,verified_at FROM fry_sender_grant WHERE organization=$1 ORDER BY scope LIMIT 1001;

-- name: StoreAuthorizeSender :one
-- Used by AuthorizeSender in sender_authority_linux.go.
SELECT id,version FROM fry_sender_grant WHERE organization=$1 AND active AND scope=ANY($2::text[]) ORDER BY CASE WHEN scope=$3 THEN 0 ELSE 1 END LIMIT 1;

-- name: GuardSenderAuthorization :one
-- Used by guardSenderAuthorization in sender_authority_linux.go.
SELECT scope,version,active FROM fry_sender_grant WHERE id=$1 AND organization=$2 FOR SHARE;

-- name: StorePairForAttempt :one
-- Used by pairForAttempt in shards_linux.go.
SELECT ingest_shard FROM fry_attempt WHERE id=$1;

-- name: BindShardsReadAdvisoryLock :execresult
-- Used by bindShards in shards_linux.go.
SELECT pg_advisory_xact_lock(1667855461,1);

-- name: BindShardsReadIngestCatalog :one
-- Used by bindShards in shards_linux.go.
SELECT catalog FROM fry_ingest_catalog WHERE singleton;

-- name: BindShardsCheckAttempt :one
-- Used by bindShards in shards_linux.go.
SELECT EXISTS(SELECT 1 FROM fry_attempt);

-- name: BindShardsInsertIngestCatalog :execresult
-- Used by bindShards in shards_linux.go.
INSERT INTO fry_ingest_catalog(singleton,version,algorithm,catalog) VALUES(true,1,'attempt-sha256-v1',$1);

-- name: BindShardsInsertIngestShard :execresult
-- Used by bindShards in shards_linux.go.
INSERT INTO fry_ingest_shard(id,member_a,member_b) VALUES($1,$2,$3);

-- name: BindShardsReadIngestCatalogVersionAlgorithm :one
-- Used by bindShards in shards_linux.go.
SELECT version,algorithm FROM fry_ingest_catalog WHERE singleton;

-- name: BindShardsReadIngestShard :many
-- Used by bindShards in shards_linux.go.
SELECT id,member_a,member_b FROM fry_ingest_shard ORDER BY id;

-- name: StoreSourceAttempt :one
-- Used by SourceAttempt in source_linux.go.
SELECT a.id,a.envelope FROM fry_source x JOIN fry_attempt a ON a.id=x.attempt WHERE x.connection=$1 AND x.folder=$2 AND x.uidvalidity=$3 AND x.uid=$4 AND a.organization=$5;

-- name: StoreAcceptedSources :many
-- Used by AcceptedSources in source_linux.go.
SELECT x.uid FROM fry_source x JOIN fry_attempt a ON a.id=x.attempt WHERE x.connection=$1 AND x.folder=$2 AND x.uidvalidity=$3 AND x.uid=ANY($4::bigint[]) AND x.accepted AND a.organization=$5;

-- name: StoreBindSourceConnectionInsertSourceConnection :execresult
-- Used by BindSourceConnection in source_linux.go.
INSERT INTO fry_source_connection(id,organization,fingerprint) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: StoreBindSourceConnectionReadSourceConnection :one
-- Used by BindSourceConnection in source_linux.go.
SELECT organization=$2 AND fingerprint=$3 FROM fry_source_connection WHERE id=$1;

-- name: PackerReconcileStagingReadPack :many
-- Used by reconcileStaging in staging_linux.go.
SELECT id FROM fry_pack WHERE staging_owner=$1 AND NOT staging_closed ORDER BY id;

-- name: PackerReconcileStagingUpdatePack :execresult
-- Used by reconcileStaging in staging_linux.go.
UPDATE fry_pack SET state='retiring',epoch=epoch+1 WHERE id=$1 AND state='building';

-- name: PackerReconcileStagingUpdatePack2 :execresult
-- Used by reconcileStaging in staging_linux.go.
UPDATE fry_pack SET staging_closed=true WHERE id=$1 AND staging_owner=$2;

-- name: CheckStorageUsageSchemaReadStorageAccounting :one
-- Used by checkStorageUsageSchema in storage_usage_linux.go.
SELECT version,algorithm FROM fry_storage_accounting WHERE singleton;

-- name: CheckStorageUsageSchemaReadStorageUsage :many
-- Used by checkStorageUsageSchema in storage_usage_linux.go.
SELECT organization,slot,retained_originals,retained_bytes FROM fry_storage_usage LIMIT 0;

-- name: AddRetainedStorage :execresult
-- Used by addRetainedStorage in storage_usage_linux.go.
INSERT INTO fry_storage_usage(organization,slot,retained_originals,retained_bytes)
 SELECT organization,$2,1,raw_size FROM fry_receipt WHERE id=$1
 ON CONFLICT(organization,slot) DO UPDATE SET retained_originals=fry_storage_usage.retained_originals+1,retained_bytes=fry_storage_usage.retained_bytes+EXCLUDED.retained_bytes;

-- name: RemoveRetainedStorage :execresult
-- Used by removeRetainedStorage in storage_usage_linux.go.
UPDATE fry_storage_usage SET retained_originals=retained_originals-1,retained_bytes=retained_bytes-$3 WHERE organization=$1 AND slot=$2 AND retained_originals>0 AND retained_bytes>=$3;

-- name: ReadOrganizationStorageUsage :one
-- Used by readOrganizationStorageUsage in storage_usage_linux.go.
SELECT COALESCE(sum(retained_originals),0)::bigint,COALESCE(sum(retained_bytes),0)::bigint FROM fry_storage_usage WHERE organization=$1;

-- name: NewWithShards :one
-- Used by NewWithShards in store_linux.go.
SELECT current_setting('fsync'),current_setting('full_page_writes');

-- name: StoreAccept :one
-- Used by Accept in store_linux.go.
SELECT state FROM fry_attempt WHERE id=$1;

-- name: StoreReadPinnedReadTarget :many
-- Used by readPinned in store_linux.go.
SELECT t.member FROM fry_target t JOIN fry_receipt r ON r.attempt=t.attempt WHERE r.id=$1 AND r.organization=$2 AND NOT t.bytes_removed ORDER BY t.member;

-- name: StoreReadPinnedReadTargetTMemberTIncarnationTLocator :many
-- Used by readPinned in store_linux.go.
SELECT t.member,t.incarnation,t.locator FROM fry_target t JOIN fry_receipt r ON r.attempt=t.attempt WHERE r.id=$1 AND r.organization=$2 AND NOT t.bytes_removed ORDER BY t.member;

-- name: GuardSubmissionAuthorization :one
-- Used by guardSubmissionAuthorization in submission_authority_linux.go.
SELECT version,enabled,authorized_senders,authorized_domains FROM fry_submission_account WHERE username=$1 AND organization=$2 FOR SHARE;

-- name: GuardPlacementReadMember :one
-- Used by guardPlacement in transport_linux.go.
SELECT incarnation,failure_domain,maintenance_job,p.enabled,p.max_message_bytes,p.max_records,p.max_file_bytes,p.lane_count FROM fry_member m JOIN fry_member_profile p ON p.member=m.id WHERE id=$1 AND EXISTS(SELECT 1 FROM fry_member_file f WHERE f.member=m.id AND f.file_id=m.incarnation AND f.directory=m.directory AND f.state='current' AND f.rewrite_job IS NULL) FOR SHARE OF m;

-- name: GuardPlacementReadMemberOwner :one
-- Used by guardPlacement in transport_linux.go.
SELECT epoch,closed FROM fry_member_owner WHERE member=$1 ORDER BY epoch DESC LIMIT 1;


-- name: RequiresObjectStorage :one
-- Startup must retain object storage access while historical packs exist.
SELECT (EXISTS(SELECT 1 FROM fry_object_namespace WHERE namespace IS NOT NULL) OR EXISTS(SELECT 1 FROM fry_pack))::boolean;

-- name: FolderByName :one
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE i.id=$1 AND i.organization=$2 AND i.migration_job IS NULL AND f.name=$3;

-- name: FolderByNameLocked :one
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE i.id=$1 AND i.organization=$2 AND i.migration_job IS NULL AND f.name=$3 FOR UPDATE OF f;

-- name: FolderByID :one
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE i.id=$1 AND i.organization=$2 AND i.migration_job IS NULL AND f.id=$3;

-- name: FolderByIDLocked :one
SELECT f.id,f.name,f.uidvalidity,f.uidnext,EXISTS(SELECT 1 FROM fry_subscription sub WHERE sub.inbox=f.inbox AND sub.name=f.name),f.keywords FROM fry_folder f JOIN fry_inbox i ON i.id=f.inbox WHERE i.id=$1 AND i.organization=$2 AND i.migration_job IS NULL AND f.id=$3 FOR UPDATE OF f;

-- name: ReceiptByID :one
SELECT id,attempt,organization,received_at,raw_size,sha256,profile_epoch FROM fry_receipt WHERE organization=$1 AND id=$2;

-- name: ReceiptByAttempt :one
SELECT id,attempt,organization,received_at,raw_size,sha256,profile_epoch FROM fry_receipt WHERE organization=$1 AND attempt=$2;

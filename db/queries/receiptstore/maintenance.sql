-- name: MaintenanceNextPackOrganization :one
SELECT r.organization FROM fry_receipt r WHERE r.lifecycle='live' AND r.pack_id IS NULL
 AND EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM fry_pack_member pm JOIN fry_pack p ON p.id=pm.pack WHERE pm.receipt=r.id AND p.state!='retired') AND r.raw_size<=$1 AND r.organization>$2 ORDER BY r.organization LIMIT 1;

-- name: MaintenanceNextCompactOrganization :one
SELECT p.organization FROM fry_pack p WHERE p.state='published' AND p.staging_closed
 AND EXISTS(SELECT 1 FROM fry_pack_member pm JOIN fry_receipt r ON r.id=pm.receipt WHERE pm.pack=p.id AND r.pack_id=p.id
  AND NOT EXISTS(SELECT 1 FROM fry_reference f WHERE f.receipt=r.id AND (f.expires_at IS NULL OR f.expires_at>clock_timestamp())))
 AND NOT EXISTS(SELECT 1 FROM fry_pack pending WHERE pending.source_pack=p.id AND pending.state IN ('building','retiring')) AND p.organization>$1 ORDER BY p.organization LIMIT 1;

-- name: MaintenanceNextRetireOrganization :one
SELECT p.organization FROM fry_pack p WHERE p.staging_closed AND (p.state='retiring' OR (p.state='building' AND p.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM fry_read_pin pin WHERE pin.pack=p.id) AND p.organization>$1 ORDER BY p.organization LIMIT 1;

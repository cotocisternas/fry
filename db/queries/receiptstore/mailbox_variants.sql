-- Finite mailbox operations retain separate statements so the existing planner
-- predicates and set/add/remove behavior stay explicit.

-- name: SetMailboxFlags :execresult
UPDATE fry_mail
SET flags=sqlc.arg(flags)::text[]
WHERE folder=sqlc.arg(folder) AND uid=ANY(sqlc.arg(uids)::bigint[]);

-- name: AddMailboxFlags :execresult
UPDATE fry_mail
SET flags=ARRAY(SELECT DISTINCT v FROM unnest(flags || sqlc.arg(flags)::text[]) AS v ORDER BY v)
WHERE folder=sqlc.arg(folder) AND uid=ANY(sqlc.arg(uids)::bigint[]);

-- name: RemoveMailboxFlags :execresult
UPDATE fry_mail
SET flags=ARRAY(SELECT v FROM unnest(flags) AS v WHERE NOT(v=ANY(sqlc.arg(flags)::text[])) ORDER BY v)
WHERE folder=sqlc.arg(folder) AND uid=ANY(sqlc.arg(uids)::bigint[]);

-- name: NextMailboxOrganization :one
SELECT w.organization
FROM fry_work w
JOIN fry_receipt r ON r.id=w.receipt AND r.organization=w.organization
JOIN fry_inbox i ON i.id=w.inbox AND i.organization=r.organization
WHERE w.available_at<=sqlc.arg(due_at) AND i.migration_job IS NULL
  AND w.organization>sqlc.arg(after_organization)
ORDER BY w.organization LIMIT 1;

-- name: WrappedMailboxOrganization :one
SELECT w.organization
FROM fry_work w
JOIN fry_receipt r ON r.id=w.receipt AND r.organization=w.organization
JOIN fry_inbox i ON i.id=w.inbox AND i.organization=r.organization
WHERE w.available_at<=sqlc.arg(due_at) AND i.migration_job IS NULL
  AND w.organization<=sqlc.arg(after_organization)
ORDER BY w.organization LIMIT 1;

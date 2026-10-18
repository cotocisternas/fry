-- sqlc analysis definitions for PostgreSQL system catalogs. Never installed.
CREATE TABLE pg_index (
 indexrelid oid NOT NULL,
 indisvalid boolean NOT NULL,
 indisready boolean NOT NULL
);
CREATE TABLE pg_class (
 oid oid NOT NULL,
 relnamespace oid NOT NULL,
 relname name NOT NULL
);

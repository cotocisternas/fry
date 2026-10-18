// Package dbschema installs a fresh FRY schema atomically and checks its frozen
// installation contract. It does not adopt, repair, or upgrade existing schemas.
package dbschema

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Component is the complete SQL definition owned by one module. Order is part
// of the installation checksum and must be identical at initialization/startup.
type Component struct {
	Name string
	SQL  string
}

const (
	installationVersion = 1
	maxCatalogEntries   = 4096
	maxCatalogBytes     = 4 << 20
	operationTimeout    = 10 * time.Second
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ConfigurePool pins all pool and copied direct-connection configurations to a
// single application schema. PostgreSQL implicitly searches pg_catalog first.
func ConfigurePool(config *pgxpool.Config) error {
	if config == nil || config.ConnConfig == nil {
		return fmt.Errorf("dbschema: missing pool configuration")
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	name := config.ConnConfig.RuntimeParams["search_path"]
	if name == "" {
		name = "public"
	}
	// Accept our own normalized output, making repeated configuration idempotent.
	if len(name) > 1 && name[0] == '"' && name[len(name)-1] == '"' {
		name = name[1 : len(name)-1]
	}
	if !validSchema(name) {
		return fmt.Errorf("dbschema: search_path must name one simple application schema")
	}
	config.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{name}.Sanitize()
	return nil
}

func validSchema(name string) bool {
	return schemaName.MatchString(name) && name != "information_schema" && !strings.HasPrefix(name, "pg_")
}

func componentChecksum(components []Component) (string, error) {
	if len(components) == 0 || len(components) > 64 {
		return "", fmt.Errorf("dbschema: invalid component count")
	}
	h := sha256.New()
	names := make(map[string]bool)
	total := 0
	writeField(h, "fry-installation-v1")
	for _, c := range components {
		total += len(c.Name) + len(c.SQL)
		if c.Name == "" || len(c.Name) > 128 || names[c.Name] || strings.TrimSpace(c.SQL) == "" ||
			total > maxCatalogBytes {
			return "", fmt.Errorf("dbschema: invalid schema components")
		}
		names[c.Name] = true
		writeField(h, c.Name)
		writeField(h, c.SQL)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])      //nolint:errcheck // hash.Hash.Write is documented to never return an error.
	_, _ = h.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write is documented to never return an error.
}

func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.ErrorContext(ctx, "schema transaction rollback failed")
	}
}

// target validates the effective schema as well as its literal setting: silently
// ignored schemas and extra search-path fallbacks are not an installation scope.
func target(ctx context.Context, tx pgx.Tx) (string, int, error) {
	var setting string
	var schemas []string
	var major int
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('search_path'),pg_catalog.current_schemas(false),p"+
		"g_catalog.current_setting('server_version_num')::integer/10000").
		Scan(&setting, &schemas, &major); err != nil {
		return "", 0, err
	}
	if len(schemas) != 1 || !validSchema(schemas[0]) ||
		(setting != schemas[0] && setting != pgx.Identifier{schemas[0]}.Sanitize()) {
		return "", 0, fmt.Errorf("dbschema: search_path must resolve to exactly one application schema")
	}
	// Pin deparser visibility for stable pg_get_* output within this transaction.
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_catalog.set_config('search_path',$1,true)`,
		pgx.Identifier{schemas[0]}.Sanitize(),
	); err != nil {
		return "", 0, err
	}
	return schemas[0], major, nil
}

// Initialize permits exactly one fresh installation. Existing fry_ relations,
// types, functions, constraints, and triggers are rejected; catalog coverage is
// limited to the supported object kinds enumerated below. All component DDL and
// its ledger commit together; the transaction lock serializes initializers.
func Initialize(parent context.Context, pool *pgxpool.Pool, components []Component) error {
	checksum, err := componentChecksum(components)
	if err != nil {
		return err
	}
	if pool == nil {
		return fmt.Errorf("dbschema: missing pool")
	}
	ctx, cancel := context.WithTimeout(parent, operationTimeout)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	schema, major, err := target(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(pg_catalog.curre"+
			"nt_database() || ':' || $1, 719430161))",
		schema,
	); err != nil {
		return err
	}
	if err := installComponents(ctx, tx, schema, major, checksum, components); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("dbschema: installation commit outcome must be checked before retry: %w", err)
	}
	return nil
}

// installComponents records the catalog only after all component DDL succeeds.
// The caller owns the single installation transaction and its advisory lock.
func installComponents(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	major int,
	checksum string,
	components []Component,
) error {
	entries, err := catalog(ctx, tx, schema)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("dbschema: schema contains existing fry_ objects; fresh initialization required")
	}
	for _, c := range components {
		if _, err = tx.Exec(ctx, c.SQL); err != nil {
			return fmt.Errorf("dbschema: initialize %s: %w", c.Name, err)
		}
	}
	ledger := pgx.Identifier{schema, "fry_schema_version"}.Sanitize()
	if _, err = tx.Exec(
		ctx,
		`CREATE TABLE `+ledger+" (singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton), version integer NOT "+
			"NULL CHECK(version=1), checksum text NOT NULL CHECK(checksum ~ '^[0-9a-f]{64}$'), "+
			"catalog_signature text NOT NULL CHECK(catalog_signature ~ '^[0-9a-f]{64}$'), "+
			"pg_major integer NOT NULL CHECK(pg_major>0))",
	); err != nil {
		return err
	}
	entries, err = catalog(ctx, tx, schema)
	if err != nil {
		return err
	}
	signature := catalogChecksum(entries)
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO `+ledger+` (singleton,version,checksum,catalog_signature,pg_major) VALUES(true,$1,$2,$3,$4)`,
		installationVersion,
		checksum,
		signature,
		major,
	); err != nil {
		return err
	}
	return nil
}

// Check performs a read-only repeatable-read compatibility check. The checksum
// identifies compiled SQL; the separate catalog digest detects structural drift.
// Neither is protection against a database administrator rewriting the ledger.
func Check(parent context.Context, pool *pgxpool.Pool, components []Component) error {
	checksum, err := componentChecksum(components)
	if err != nil {
		return err
	}
	if pool == nil {
		return fmt.Errorf("dbschema: missing pool")
	}
	ctx, cancel := context.WithTimeout(parent, operationTimeout)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	schema, major, err := target(ctx, tx)
	if err != nil {
		return err
	}
	// Inspect shape before selecting the ledger so malformed installations fail
	// without depending on their row contents or resolving an unqualified table.
	entries, err := catalog(ctx, tx, schema)
	if err != nil {
		return err
	}
	if err := checkLedger(ctx, tx, schema, major, checksum, catalogChecksum(entries)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type installationRecord struct {
	singleton           bool
	version, major      int
	checksum, signature string
}

func checkLedger(ctx context.Context, tx pgx.Tx, schema string, major int, checksum, expectedSignature string) error {
	ledger := pgx.Identifier{schema, "fry_schema_version"}.Sanitize()
	rows, err := tx.Query(ctx, `SELECT singleton,version,checksum,catalog_signature,pg_major FROM `+ledger+` LIMIT 2`)
	if err != nil {
		return fmt.Errorf("dbschema: installation ledger unavailable: %w", err)
	}
	var got installationRecord
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&got.singleton, &got.version, &got.checksum, &got.signature, &got.major); err != nil {
			break
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	want := installationRecord{
		singleton: true,
		version:   installationVersion,
		checksum:  checksum,
		signature: expectedSignature,
		major:     major,
	}
	if count != 1 || got != want {
		return fmt.Errorf("dbschema: incompatible installation version, components, PostgreSQL major, or catalog")
	}
	return nil
}

func catalogChecksum(entries []string) string {
	sort.Strings(entries)
	h := sha256.New()
	writeField(h, "fry-catalog-v1")
	for _, entry := range entries {
		writeField(h, entry)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func catalog(ctx context.Context, tx pgx.Tx, schema string) ([]string, error) {
	rows, err := tx.Query(ctx, catalogSQL, schema, maxCatalogEntries+1, maxCatalogBytes+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]string, 0, 256)
	total := 0
	for rows.Next() {
		var entry string
		if err = rows.Scan(&entry); err != nil {
			return nil, err
		}
		total += len(entry)
		if len(entries) >= maxCatalogEntries || total > maxCatalogBytes {
			return nil, fmt.Errorf("dbschema: catalog exceeds installation bounds")
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// Use explicit prefix comparisons: LIKE 'fry_%' would treat '_' as a wildcard.
// Names and definitions, not OIDs/owners/row data/sequence positions, form the
// signature. Dependencies attached to FRY relations are included even if renamed.
const catalogSQL = "WITH relations AS (\n SELECT c.* FROM pg_catalog.pg_class c JOIN " +
	"pg_catalog.pg_namespace n ON n.oid=c.relnamespace\n WHERE n.nspname=$1 AND " +
	"left(c.relname,4)='fry_'\n), entries AS (\n SELECT " +
	"jsonb_build_array('relation',c.relname,c.relkind,c.relpersistence,c.relreplident,c.r" +
	"elrowsecurity,c.relforcerowsecurity,c.reloptions,\n CASE WHEN c.relkind IN " +
	"('v','m') THEN pg_catalog.pg_get_viewdef(c.oid,false) ELSE NULL END)::text AS entry " +
	"FROM relations c\n UNION ALL\n SELECT jsonb_build_array('column',c.relname,a.attnum," +
	"a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attidentity," +
	"a.attgenerated,a.attisdropped,a.attstorage,a.attcompression,\n " +
	"co.collname,cn.nspname,pg_catalog.pg_get_expr(d.adbin,d.adrelid,false))::text\n " +
	"FROM relations c JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum>0\n" +
	" LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum\n LEFT " +
	"JOIN pg_catalog.pg_collation co ON co.oid=a.attcollation LEFT JOIN " +
	"pg_catalog.pg_namespace cn ON cn.oid=co.collnamespace\n UNION ALL\n SELECT " +
	"jsonb_build_array('constraint',r.relname,k.conname,k.contype,k.convalidated,k.condef" +
	"errable,k.condeferred,pg_catalog.pg_get_constraintdef(k.oid,false))::text\n FROM " +
	"pg_catalog.pg_constraint k JOIN pg_catalog.pg_namespace n ON n.oid=k.connamespace " +
	"LEFT JOIN pg_catalog.pg_class r ON r.oid=k.conrelid\n WHERE n.nspname=$1 AND " +
	"(left(k.conname,4)='fry_' OR k.conrelid IN (SELECT oid FROM relations))\n UNION " +
	"ALL\n SELECT jsonb_build_array('index',r.relname,i.relname,x.indisvalid,x.indisready" +
	",x.indislive,x.indisreplident,pg_catalog.pg_get_indexdef(i.oid,0,false))::text\n " +
	"FROM pg_catalog.pg_index x JOIN pg_catalog.pg_class i ON i.oid=x.indexrelid JOIN " +
	"pg_catalog.pg_class r ON r.oid=x.indrelid\n WHERE x.indrelid IN (SELECT oid FROM " +
	"relations) OR x.indexrelid IN (SELECT oid FROM relations)\n UNION ALL\n SELECT " +
	"jsonb_build_array('sequence',r.relname,pg_catalog.format_type(s.seqtypid,NULL),s.seq" +
	"start,s.seqincrement,s.seqmax,s.seqmin,s.seqcache,s.seqcycle)::text\n FROM " +
	"pg_catalog.pg_sequence s JOIN relations r ON r.oid=s.seqrelid\n UNION ALL\n SELECT " +
	"jsonb_build_array('trigger',r.relname,t.tgname,t.tgenabled,pg_catalog.pg_get_trigger" +
	"def(t.oid,false))::text\n FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class r " +
	"ON r.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace\n WHERE " +
	"NOT t.tgisinternal AND n.nspname=$1 AND (left(t.tgname,4)='fry_' OR t.tgrelid IN " +
	"(SELECT oid FROM relations))\n UNION ALL\n SELECT " +
	"jsonb_build_array('function',p.proname,pg_catalog.pg_get_function_identity_arguments" +
	"(p.oid),p.prokind,p.prosrc,p.probin,p.provolatile,p.proisstrict,p.prosecdef,p.procon" +
	"fig,pg_catalog.pg_get_function_result(p.oid))::text\n FROM pg_catalog.pg_proc p " +
	"JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND " +
	"left(p.proname,4)='fry_'\n UNION ALL\n SELECT " +
	"jsonb_build_array('type',t.typname,t.typtype,pg_catalog.format_type(t.typbasetype,t." +
	"typtypmod),t.typnotnull,t.typdefault,\n (SELECT jsonb_agg(e.enumlabel ORDER BY " +
	"e.enumsortorder) FROM pg_catalog.pg_enum e WHERE e.enumtypid=t.oid))::text\n FROM " +
	"pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE " +
	"n.nspname=$1 AND left(t.typname,4)='fry_'\n) SELECT left(entry,$3) FROM entries " +
	"LIMIT $2"

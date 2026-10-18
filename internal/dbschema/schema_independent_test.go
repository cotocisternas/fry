package dbschema_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func schemaFixture(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL via scripts/tests/receipts.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	control, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(control.Close)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema := "schema_contract_" + hex.EncodeToString(nonce[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := control.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	if err := dbschema.ConfigurePool(cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func components() []dbschema.Component {
	return []dbschema.Component{
		{
			Name: "receipt",
			SQL: "CREATE TABLE fry_original(id bigserial PRIMARY KEY, body_bytes bigint NOT NULL " +
				"DEFAULT 0, label text NOT NULL, CONSTRAINT fry_positive CHECK(body_bytes>=0)); " +
				"CREATE UNIQUE INDEX fry_original_label ON fry_original(label);",
		},
		{
			Name: "admin",
			SQL: "CREATE TABLE fry_access(original bigint REFERENCES fry_original(id), enabled " +
				"boolean NOT NULL DEFAULT true);",
		},
		{
			Name: "directory",
			SQL: "CREATE TABLE fry_address(address text PRIMARY KEY, original bigint REFERENCES " +
				"fry_original(id));",
		},
	}
}

func requireCompatible(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	if err := dbschema.Check(ctx, p, components()); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaInitializationIsAtomicAcrossComponents(t *testing.T) {
	p, ctx := schemaFixture(t)
	parts := components()
	parts[2].SQL += ` SELECT definitely_missing_initializer_function();`
	if err := dbschema.Initialize(ctx, p, parts); err == nil {
		t.Fatal("late invalid SQL initialized")
	}
	var count int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND "+
		"relname LIKE 'fry_%'").
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed initialization retained %d objects", count)
	}
	if err := dbschema.Initialize(ctx, p, components()); err != nil {
		t.Fatal(err)
	}
	requireCompatible(t, ctx, p)
	if err := dbschema.Initialize(ctx, p, components()); err == nil {
		t.Fatal("repeated initialization adopted existing installation")
	}
}

func TestSchemaConcurrentInitializationHasOneCompleteWinner(t *testing.T) {
	p, ctx := schemaFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			results <- dbschema.Initialize(ctx, p, components())
		})
	}
	close(start)
	workers.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful initializers=%d", success)
	}
	requireCompatible(t, ctx, p)
}

func TestSchemaCheckPreservesDataAndSequencePositions(t *testing.T) {
	p, ctx := schemaFixture(t)
	if err := dbschema.Initialize(ctx, p, components()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(
		ctx,
		"INSERT INTO fry_original(label,body_bytes) VALUES('saved',42); SELECT "+
			"setval('fry_original_id_seq',87,true)",
	); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		requireCompatible(t, ctx, p)
	}
	requireSavedDataAndSequence(t, ctx, p)
	if _, err := p.Exec(
		ctx,
		`UPDATE fry_original SET body_bytes=43; SELECT nextval('fry_original_id_seq')`,
	); err != nil {
		t.Fatal(err)
	}
	requireCompatible(t, ctx, p)
}

func requireSavedDataAndSequence(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	var label string
	var size, position int64
	var called bool
	if err := p.QueryRow(ctx, `SELECT label,body_bytes FROM fry_original`).Scan(&label, &size); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT last_value,is_called FROM fry_original_id_seq`).
		Scan(&position, &called); err != nil {
		t.Fatal(err)
	}
	if label != "saved" || size != 42 || position != 87 || !called {
		t.Fatalf("Check mutated authority: %q %d %d %v", label, size, position, called)
	}
}

func TestSchemaCheckRejectsStructuralDrift(t *testing.T) {
	for name, ddl := range map[string]string{
		"constraint":          `ALTER TABLE fry_original DROP CONSTRAINT fry_positive`,
		"index":               `DROP INDEX fry_original_label`,
		"column":              `ALTER TABLE fry_original DROP COLUMN body_bytes`,
		"default":             `ALTER TABLE fry_original ALTER COLUMN body_bytes SET DEFAULT 9`,
		"sequence definition": `ALTER SEQUENCE fry_original_id_seq INCREMENT BY 2`,
		"added object":        `CREATE TABLE fry_unrecognized(id integer)`,
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx := schemaFixture(t)
			if err := dbschema.Initialize(ctx, p, components()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := dbschema.Check(ctx, p, components()); err == nil {
				t.Fatal("structural drift accepted")
			}
		})
	}
}

func applyFixtureDDL(t *testing.T, ctx context.Context, p *pgxpool.Pool, ddl string) {
	t.Helper()
	if ddl == "" {
		return
	}
	if _, err := p.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaRejectsMissingPartialAndUnversionedObjects(t *testing.T) {
	for name, ddl := range map[string]string{
		"empty":                "",
		"partial":              `CREATE TABLE fry_original(id bigint)`,
		"foreign fry sequence": `CREATE SEQUENCE fry_orphan`,
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx := schemaFixture(t)
			applyFixtureDDL(t, ctx, p, ddl)
			if err := dbschema.Check(ctx, p, components()); err == nil {
				t.Fatal("unversioned schema accepted")
			}
			if ddl != "" {
				if err := dbschema.Initialize(ctx, p, components()); err == nil {
					t.Fatal("existing fry object adopted")
				}
			}
		})
	}
}

func TestSchemaLedgerCorruptionAndArtifactChangesFailClosed(t *testing.T) {
	for name, mutation := range map[string]string{
		"checksum":             `UPDATE fry_schema_version SET checksum=repeat('0',64)`,
		"catalog signature":    `UPDATE fry_schema_version SET catalog_signature=repeat('0',64)`,
		"postgres major":       `UPDATE fry_schema_version SET pg_major=1`,
		"missing ledger row":   `DELETE FROM fry_schema_version`,
		"missing ledger table": `DROP TABLE fry_schema_version`,
		// Remove only the version CHECK in this corrupt fixture so an unsupported
		// persisted version reaches Check, rather than merely testing PostgreSQL.
		"version": "DO $$DECLARE c text; BEGIN FOR c IN SELECT conname FROM pg_constraint WHERE " +
			"conrelid='fry_schema_version'::regclass AND contype='c' LOOP EXECUTE format('ALTER " +
			"TABLE fry_schema_version DROP CONSTRAINT %I',c); END LOOP; END$$; UPDATE " +
			"fry_schema_version SET version=2",
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx := schemaFixture(t)
			if err := dbschema.Initialize(ctx, p, components()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Exec(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if err := dbschema.Check(ctx, p, components()); err == nil {
				t.Fatal("corrupt ledger accepted")
			}
		})
	}
	t.Run("different artifact", func(t *testing.T) {
		p, ctx := schemaFixture(t)
		if err := dbschema.Initialize(ctx, p, components()); err != nil {
			t.Fatal(err)
		}
		changed := components()
		changed[0].SQL += "\n-- changed installation artifact\n"
		if err := dbschema.Check(ctx, p, changed); err == nil {
			t.Fatal("different SQL artifact accepted")
		}
	})
}

func poolConfigWithSearchPath(t *testing.T, path string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://localhost/postgres")
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = path
	}
	return cfg
}

func TestSchemaPoolConfigurationRejectsFallbackSearchPaths(t *testing.T) {
	for _, path := range []string{
		`public,other`,
		`"$user", public`,
		`pg_catalog`,
		`pg_temp`,
		`information_schema`,
		`UPPER`,
		`a.b`,
		`"public",public`,
		`public;select 1`,
		string(make([]byte, 64)),
	} {
		t.Run(path, func(t *testing.T) {
			cfg := poolConfigWithSearchPath(t, path)
			if err := dbschema.ConfigurePool(cfg); err == nil {
				t.Fatal("unsafe search_path accepted")
			}
		})
	}
	for _, path := range []string{"", "public", "isolated_test"} {
		t.Run("safe "+path, func(t *testing.T) {
			cfg := poolConfigWithSearchPath(t, path)
			if err := dbschema.ConfigurePool(cfg); err != nil {
				t.Fatal(err)
			}
			first := cfg.ConnConfig.RuntimeParams["search_path"]
			if err := dbschema.ConfigurePool(cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ConnConfig.RuntimeParams["search_path"] != first {
				t.Fatal("configuration not idempotent")
			}
		})
	}
}

func TestSchemaCheckRejectsRuntimeSearchPathShadowing(t *testing.T) {
	p, ctx := schemaFixture(t)
	if err := dbschema.Initialize(ctx, p, components()); err != nil {
		t.Fatal(err)
	}
	cfg := p.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] += ",public"
	shadow, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()
	if err := dbschema.Check(ctx, shadow, components()); err == nil {
		t.Fatal("runtime fallback search_path accepted")
	}
}

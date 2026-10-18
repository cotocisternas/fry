package telemetry

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ConfigurePool installs tracing before the pool is created. Existing tracers
// are rejected rather than silently replaced, since they may own other hooks.
func (r *Runtime) ConfigurePool(cfg *pgxpool.Config) error {
	if cfg == nil || cfg.ConnConfig == nil {
		return errors.New("telemetry: missing pool configuration")
	}
	if r == nil || r.tracer == nil {
		return nil
	}
	if cfg.ConnConfig.Tracer != nil {
		return errors.New("telemetry: pool already has a query tracer")
	}
	cfg.ConnConfig.Tracer = queryTracer{runtime: r}
	return nil
}

type (
	queryTracer struct{ runtime *Runtime }
	queryKey    struct{}
	queryState  struct{ finish func(error) }
)

func (t queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	verb := sqlVerb(data.SQL)
	ctx, finish := t.runtime.start(
		ctx,
		"postgresql."+strings.ToLower(verb),
		[]attribute.KeyValue{
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", verb),
		},
		trace.WithSpanKind(trace.SpanKindClient),
	)
	return context.WithValue(ctx, queryKey{}, queryState{finish: finish})
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if state, ok := ctx.Value(queryKey{}).(queryState); ok {
		state.finish(data.Err)
	}
}

// sqlVerb recognizes a bounded enum after leading SQL/sqlc comments. It never
// returns a substring from the query and does not retain arguments or SQL text.
func sqlVerb(sql string) string {
	if len(sql) > 8192 {
		sql = sql[:8192]
	}
	sql = stripSQLComments(sql)
	end := 0
	for end < len(sql) && (sql[end] >= 'a' && sql[end] <= 'z' || sql[end] >= 'A' && sql[end] <= 'Z') {
		end++
	}
	return knownSQLVerb(strings.ToUpper(sql[:end]))
}

func stripSQLComments(sql string) string {
	for {
		sql = strings.TrimLeftFunc(sql, unicode.IsSpace)
		if strings.HasPrefix(sql, "--") {
			end := strings.IndexByte(sql, '\n')
			if end < 0 {
				return ""
			}
			sql = sql[end+1:]
			continue
		}
		if strings.HasPrefix(sql, "/*") {
			end, ok := blockCommentEnd(sql)
			if !ok {
				return ""
			}
			sql = sql[end:]
			continue
		}
		break
	}
	return sql
}

func blockCommentEnd(sql string) (int, bool) {
	depth, end := 1, 2
	for end < len(sql) && depth > 0 {
		switch {
		case strings.HasPrefix(sql[end:], "/*"):
			depth++
			end += 2
		case strings.HasPrefix(sql[end:], "*/"):
			depth--
			end += 2
		default:
			end++
		}
	}
	return end, depth == 0
}

func knownSQLVerb(verb string) string {
	verbs := map[string]string{
		"SELECT": "SELECT", "INSERT": "INSERT", "UPDATE": "UPDATE", "DELETE": "DELETE",
		"WITH": "WITH", "BEGIN": "BEGIN", "COMMIT": "COMMIT", "ROLLBACK": "ROLLBACK",
		"SET": "SET", "SHOW": "SHOW", "CREATE": "CREATE", "ALTER": "ALTER", "DROP": "DROP", "EXPLAIN": "EXPLAIN",
	}
	if fixed, ok := verbs[verb]; ok {
		return fixed
	}
	return "OTHER"
}

package backfill

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultLegacySchema is where the rehearsal and the cutover restore the
// Python tables: a dump of Supabase's public schema, restored into the Go
// database under this name. See docs/rehearsal.md.
const DefaultLegacySchema = "legacy"

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// source names the schema the legacy tables are read from: legacy in the Go
// database, where a dump of Supabase is restored, or public when reading a
// restored copy of Supabase itself.
type source struct {
	schema string
	shapes map[string]tableShape
}

func newSource(schema string) (source, error) {
	if schema == "" {
		schema = DefaultLegacySchema
	}
	if !schemaName.MatchString(schema) {
		return source{}, fmt.Errorf("legacy schema %q is not a plain lower-case identifier", schema)
	}
	return source{schema: schema}, nil
}

func (s source) table(name string) string {
	return pgx.Identifier{s.schema, name}.Sanitize()
}

// tableShape is what the source actually has, read once per run. The Python
// tables grew by hand-run migrations, so a column the code expects may not
// exist on every project; reading the shape keeps one missing column from
// failing a whole pass.
type tableShape struct {
	exists  bool
	columns map[string]string
}

func (t tableShape) has(column string) bool {
	_, ok := t.columns[column]
	return ok
}

// col is the legacy column, or NULL when this source never had it.
func (t tableShape) col(column string) string {
	if t.has(column) {
		return "l." + column
	}
	return "NULL"
}

// environmentFilter drops the rows the development stack wrote into the shared
// Supabase project. Only tables that carry the column can be filtered.
func (t tableShape) environmentFilter() string {
	if t.has("environment") {
		return ` AND l.environment IS DISTINCT FROM 'development'`
	}
	return ""
}

// changed is when a legacy row last moved: updated_at where the table keeps
// one, created_at where it does not. A table with neither never appears in a
// delta.
func (t tableShape) changed() string {
	present := []string{}
	for _, column := range []string{"updated_at", "created_at"} {
		if t.has(column) {
			present = append(present, "l."+column)
		}
	}
	switch len(present) {
	case 0:
		return "NULL::timestamptz"
	case 1:
		return present[0]
	}
	return "coalesce(" + strings.Join(present, ", ") + ")"
}

func (s *source) describe(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT table_name::text, column_name::text, data_type::text
		FROM information_schema.columns
		WHERE table_schema = $1`, s.schema)
	if err != nil {
		return fmt.Errorf("reading the legacy table shapes: %w", err)
	}
	defer rows.Close()

	s.shapes = map[string]tableShape{}
	for rows.Next() {
		var table, column, dataType string
		if err := rows.Scan(&table, &column, &dataType); err != nil {
			return fmt.Errorf("reading the legacy table shapes: %w", err)
		}
		shape, ok := s.shapes[table]
		if !ok {
			shape = tableShape{exists: true, columns: map[string]string{}}
		}
		shape.columns[column] = dataType
		s.shapes[table] = shape
	}
	return rows.Err()
}

func (s source) shape(table string) tableShape {
	return s.shapes[table]
}

// require fails loudly for a table the backfill cannot do without. A wrong
// --legacy-schema should stop the run, not report an empty migration.
func (s source) require(tables ...string) error {
	for _, table := range tables {
		if !s.shapes[table].exists {
			return fmt.Errorf("the legacy table %s does not exist", s.table(table))
		}
	}
	return nil
}

const uuidRegex = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

const uuidPattern = `'` + uuidRegex + `'`

// uuidShape is uuidPattern for a value already read into Go, so an owner is
// judged a UUID by the same rule on both sides.
var uuidShape = regexp.MustCompile(uuidRegex)

// uuidOrNull turns a legacy text user id into a UUID, or NULL when it is not
// one, so the NOT NULL on the canonical column rejects the row by name.
func uuidOrNull(expression string) string {
	return "(CASE WHEN (" + expression + ")::text ~ " + uuidPattern +
		" THEN (" + expression + ")::text::uuid END)"
}

// keyExpression is the canonical UUID a legacy key becomes. A UUID key is kept
// as it is. Any other key, a bigserial or a hand-typed text id, becomes the md5
// of the table and the key: stable across runs, so a rerun lands on the row it
// wrote before.
func keyExpression(table, column string, shape tableShape) string {
	switch shape.columns[column] {
	case "uuid":
		return "l." + column
	case "text", "character varying":
		return "(CASE WHEN l." + column + " ~ " + uuidPattern + " THEN l." + column +
			"::uuid ELSE md5('" + table + ":' || l." + column + ")::uuid END)"
	default:
		return "md5('" + table + ":' || l." + column + "::text)::uuid"
	}
}

// keyCast is how a key read back as text is compared with its own column, so
// the lookup can use the source's primary key index.
func keyCast(shape tableShape, column string) (string, error) {
	switch dataType := shape.columns[column]; dataType {
	case "uuid", "bigint", "integer", "text":
		return dataType, nil
	case "character varying":
		return "text", nil
	default:
		return "", fmt.Errorf("unsupported legacy key type %q for %s", dataType, column)
	}
}

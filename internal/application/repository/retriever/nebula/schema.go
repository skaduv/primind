package nebula

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/primind/primind/internal/types"
)

func structNamespaceProbe() types.NameSpace {
	return types.NameSpace{KnowledgeBase: "__schema_probe__", Knowledge: "__schema_probe__"}
}

// ErrSchema reports a missing, incompatible or not-yet-ready schema.
var ErrSchema = errors.New("nebula schema is incompatible or not ready")

func definitionNames() []string {
	return append([]string{entityTag, relationTag, sourceTag}, edgeNames...)
}

func definition(name string) (string, []string) {
	if fields, ok := tagFields[name]; ok {
		return "TAG", fields
	}
	return "EDGE", []string{"kb", "doc"}
}

// SchemaDDL returns the tag, edge and index definitions the adapter requires.
func SchemaDDL() []string {
	var out []string
	for _, name := range definitionNames() {
		kind, fields := definition(name)
		var cols []string
		for _, f := range fields {
			cols = append(cols, f+" string NOT NULL")
		}
		out = append(out, "CREATE "+kind+" "+name+"("+strings.Join(cols, ",")+");")
	}
	for _, name := range definitionNames() {
		kind, _ := definition(name)
		out = append(out, "CREATE "+kind+" INDEX "+name+"_scope ON "+name+"(kb(128),doc(128));")
	}
	return out
}

func fieldIndex(t wireTable, name string) int {
	for i, n := range t.Columns {
		if strings.EqualFold(n, name) {
			return i
		}
	}
	return -1
}

func cell(row []string, index int) (string, bool) {
	if index < 0 || index >= len(row) {
		return "", false
	}
	return row[index], true
}

func normalizedDDL(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), ""))
}

func (r *Repository) checkSpace(ctx context.Context) error {
	t, err := r.read(ctx, "schema", "DESCRIBE SPACE "+r.c.cfg.Space+";")
	if err != nil {
		return err
	}
	if len(t.Rows) != 1 {
		return ErrSchema
	}
	vid, ok := cell(t.Rows[0], fieldIndex(t, "Vid Type"))
	if !ok || strings.ToUpper(vid) != fmt.Sprintf("FIXED_STRING(%d)", r.c.cfg.VIDBytes) {
		return fmt.Errorf("%w: exact VID type/length", ErrSchema)
	}
	return nil
}

func (r *Repository) checkDefinitions(ctx context.Context) error {
	for _, name := range definitionNames() {
		kind, fields := definition(name)
		t, err := r.read(ctx, "schema", "DESCRIBE "+kind+" "+name+";")
		if err != nil {
			return err
		}
		if len(t.Rows) != len(fields) {
			return ErrSchema
		}
		seen := map[string]bool{}
		for _, row := range t.Rows {
			field, ok1 := cell(row, fieldIndex(t, "Field"))
			typ, ok2 := cell(row, fieldIndex(t, "Type"))
			nullable, ok3 := cell(row, fieldIndex(t, "Null"))
			if !ok1 || !ok2 || !ok3 || typ != "string" || nullable != "NO" || seen[field] {
				return ErrSchema
			}
			seen[field] = true
		}
		for _, f := range fields {
			if !seen[f] {
				return ErrSchema
			}
		}
		if err := r.checkTTL(ctx, kind, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) checkTTL(ctx context.Context, kind, name string) error {
	t, err := r.read(ctx, "schema", "SHOW CREATE "+kind+" "+name+";")
	if err != nil {
		return err
	}
	if len(t.Rows) != 1 {
		return ErrSchema
	}
	ddl, ok := cell(t.Rows[0], fieldIndex(t, "Create "+kind))
	if !ok {
		return ErrSchema
	}
	normalized := normalizedDDL(ddl)
	if !strings.Contains(normalized, "ttl_duration=0") || !strings.Contains(normalized, "ttl_col=\"\"") {
		return fmt.Errorf("%w: TTL must be disabled", ErrSchema)
	}
	return nil
}

// CheckSchema verifies the Space, definitions, TTL settings and indexes.
func (r *Repository) CheckSchema(ctx context.Context) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	if err := r.checkSpace(ctx); err != nil {
		return err
	}
	if err := r.checkDefinitions(ctx); err != nil {
		return err
	}
	for _, name := range definitionNames() {
		kind, _ := definition(name)
		ix, err := r.read(ctx, "schema", "SHOW CREATE "+kind+" INDEX "+name+"_scope;")
		if err != nil {
			return err
		}
		if len(ix.Rows) != 1 {
			return ErrSchema
		}
		ddl, ok := cell(ix.Rows[0], fieldIndex(ix, "Create "+kind+" Index"))
		if !ok {
			return ErrSchema
		}
		if !strings.Contains(normalizedDDL(ddl), "on"+name+"(kb(128),doc(128))") {
			return fmt.Errorf("%w: scope index", ErrSchema)
		}
		query := lookup(r.c.cfg, name, structNamespaceProbe())
		if kind == "EDGE" {
			query = edgeLookup(r.c.cfg, name, structNamespaceProbe())
		}
		if _, err := r.read(ctx, "schema", query); err != nil {
			return err
		}
	}
	return nil
}

// SchemaAuthorization is the explicit consent needed to create the schema.
type SchemaAuthorization struct {
	Space                       string
	CreateInEmptyDedicatedSpace bool
}

// InitializeSchema creates the schema in an existing, empty, dedicated Space.
func InitializeSchema(ctx context.Context, cfg Config, a SchemaAuthorization) error {
	if a.Space != cfg.Space || !a.CreateInEmptyDedicatedSpace {
		return fmt.Errorf("%w: explicit schema authorization required", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	defer cancel()
	r, err := pools.open(ctx, cfg, newSDKDriver)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close(context.Background()) }()
	return r.initializeEmptySchema(ctx)
}

func (r *Repository) initializeEmptySchema(ctx context.Context) error {
	if err := r.checkSpace(ctx); err != nil {
		return err
	}
	for _, stmt := range []string{"SHOW TAGS;", "SHOW EDGES;"} {
		t, err := r.read(ctx, "schema", stmt)
		if err != nil {
			return err
		}
		if len(t.Rows) > 0 {
			return fmt.Errorf("%w: automatic creation requires empty Space", ErrSchema)
		}
	}
	definitions, indexes := []batch{}, []batch{}
	for _, stmt := range SchemaDDL() {
		b := batch{Kind: "schema", Statement: stmt}
		if strings.Contains(stmt, " INDEX ") {
			indexes = append(indexes, b)
		} else {
			definitions = append(definitions, b)
		}
	}
	if err := r.runPlan(ctx, "CreateDefinitions", definitions); err != nil {
		return err
	}
	if err := r.waitReady(ctx, r.checkDefinitions); err != nil {
		return err
	}
	if err := r.runPlan(ctx, "CreateIndexes", indexes); err != nil {
		return err
	}
	return r.waitReady(ctx, r.CheckSchema)
}

func (r *Repository) waitReady(ctx context.Context, check func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, r.c.cfg.ReadyTimeout)
	defer cancel()
	var last error
	for {
		if last = check(bounded); last == nil {
			return nil
		}
		timer := time.NewTimer(r.c.cfg.PollInterval)
		select {
		case <-bounded.Done():
			timer.Stop()
			return fmt.Errorf("schema readiness expired: %w", errors.Join(bounded.Err(), last))
		case <-timer.C:
		}
	}
}

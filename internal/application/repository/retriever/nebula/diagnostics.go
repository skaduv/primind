package nebula

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/primind/primind/internal/types"
)

// ServiceEvidence is one read-only service inventory row.
type ServiceEvidence struct {
	Role    string
	Columns []string
	Rows    [][]string
}

// InspectService reads service versions and hosts for run evidence.
func (r *Repository) InspectService(ctx context.Context) ([]ServiceEvidence, error) {
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	var out []ServiceEvidence
	for _, role := range []string{"GRAPH", "META", "STORAGE"} {
		t, err := r.read(ctx, "version", "SHOW HOSTS "+role+";")
		if err != nil {
			return out, err
		}
		col := fieldIndex(t, "Version")
		if col < 0 || len(t.Rows) == 0 {
			return out, fmt.Errorf("%w: service version evidence absent", ErrInvalid)
		}
		for _, row := range t.Rows {
			version, ok := cell(row, col)
			if !ok || (version != "3.8.0" && !strings.HasPrefix(version, "3.8.0-")) {
				return out, fmt.Errorf("%w: service version differs", ErrInvalid)
			}
		}
		out = append(out, ServiceEvidence{role, t.Columns, t.Rows})
	}
	return out, nil
}

// ScopePlans returns the query plans of the namespace-scoped lookups.
func (r *Repository) ScopePlans(ctx context.Context, ns types.NameSpace) (map[string]json.RawMessage, error) {
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	if err := validateNamespace(ns, false); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, tag := range definitionNames() {
		query := lookup(r.c.cfg, tag, ns)
		if kind, _ := definition(tag); kind == "EDGE" {
			query = edgeLookup(r.c.cfg, tag, ns)
		}
		t, err := r.read(ctx, "profile", "PROFILE "+query)
		if err != nil {
			return nil, err
		}
		if len(t.PlanJSON) == 0 {
			return nil, fmt.Errorf("%w: missing PROFILE plan", ErrInvalid)
		}
		out[tag] = t.PlanJSON
	}
	return out, nil
}

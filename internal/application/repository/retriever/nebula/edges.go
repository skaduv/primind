package nebula

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/primind/primind/internal/types"
)

var edgeColumns = []string{"src", "dst", "rank", "kb", "doc"}

type storedEdge struct{ Kind, Source, Target string }

func edgeLookup(cfg Config, kind string, ns types.NameSpace) string {
	where := kind + ".kb == " + literal(encoded(ns.KnowledgeBase))
	if ns.Knowledge != "" {
		where += " AND " + kind + ".doc == " + literal(encoded(ns.Knowledge))
	}
	return "LOOKUP ON " + kind + " WHERE " + where + " YIELD src(edge) AS src,dst(edge) AS dst,rank(edge) AS rank," +
		kind + ".kb AS kb," + kind + ".doc AS doc | LIMIT " + fmt.Sprint(cfg.SnapshotRecords+1) + ";"
}

func parseIdentity(cfg Config, id string) ([]string, error) {
	if !strings.HasPrefix(id, "ng1_") || len(id) > cfg.VIDBytes {
		return nil, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "ng1_"))
	if err != nil {
		return nil, ErrInvalid
	}
	var fields []string
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) < 4 {
		return nil, ErrInvalid
	}
	ns := types.NameSpace{KnowledgeBase: fields[1], Knowledge: fields[2]}
	if err := validateNamespace(ns, true); err != nil {
		return nil, err
	}
	n := map[string]int{"entity": 4, "relation": 6, "source": 5}[fields[0]]
	if len(fields) != n {
		return nil, ErrInvalid
	}
	for _, s := range fields[3:] {
		if err := nonempty(s); err != nil {
			return nil, err
		}
	}
	expected, err := identity(cfg, fields[0], ns, fields[3:]...)
	if err != nil || id != expected {
		return nil, ErrInvalid
	}
	return fields, nil
}

func (r *Repository) rawEdges(ctx context.Context, ns types.NameSpace) ([]storedEdge, error) {
	var out []storedEdge
	seen := map[string]bool{}
	bytes := 0
	for _, kind := range edgeNames {
		tb, err := r.read(ctx, "edge_snapshot", edgeLookup(r.c.cfg, kind, ns))
		if err != nil {
			return nil, err
		}
		if err := columns(tb, edgeColumns); err != nil {
			return nil, err
		}
		for _, row := range tb.Rows {
			if len(row) != 5 || row[2] != "0" {
				return nil, ErrInvalid
			}
			kb, err := decoded(row[3])
			if err != nil {
				return nil, err
			}
			doc, err := decoded(row[4])
			if err != nil {
				return nil, err
			}
			if kb != ns.KnowledgeBase || (ns.Knowledge != "" && doc != ns.Knowledge) {
				return nil, ErrInvalid
			}
			src, err := parseIdentity(r.c.cfg, row[0])
			if err != nil {
				return nil, err
			}
			dst, err := parseIdentity(r.c.cfg, row[1])
			if err != nil {
				return nil, err
			}
			if src[1] != kb || src[2] != doc || dst[1] != kb || dst[2] != doc || dst[0] != "entity" {
				return nil, ErrInvalid
			}
			switch kind {
			case "primind_from":
				if src[0] != "relation" || src[3] != dst[3] {
					return nil, ErrInvalid
				}
			case "primind_to":
				if src[0] != "relation" || src[4] != dst[3] {
					return nil, ErrInvalid
				}
			case "primind_owner":
				if src[0] != "source" || src[3] != dst[3] {
					return nil, ErrInvalid
				}
			}
			key := kind + "|" + row[0] + "|" + row[1]
			if seen[key] {
				return nil, ErrInvalid
			}
			seen[key] = true
			bytes += len(key) + len(kb) + len(doc)
			if len(out) >= r.c.cfg.SnapshotRecords || bytes > r.c.cfg.ReadBytes {
				return nil, ErrBudget
			}
			out = append(out, storedEdge{kind, row[0], row[1]})
		}
	}
	return out, nil
}

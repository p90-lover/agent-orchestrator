package gen_test

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// growingTables hold every chat's history and grow without bound. A query that
// walks one of them from end to end reads the whole of it on every call: on
// 2026-11-12 the drain behind a new chat's opening prompt joined
// conversation_messages on turn_id alone, scanned every message of every chat,
// and left new agents waiting past four minutes for their first message.
var growingTables = map[string]bool{
	"conversation_messages":        true,
	"conversation_activities":      true,
	"conversation_provider_events": true,
	"conversation_turns":           true,
}

var planStep = regexp.MustCompile(`^(?:SCAN|SEARCH) (\w+)`)

// Every generated query is planned against the migrated schema. None may scan a
// growing table without an index, or have SQLite build an automatic index over
// one (that reads the whole table on every run too); scope the read by its
// conversation, session or branch instead, so it costs what that one chat holds.
func TestNoQueryScansAWholeChatHistoryTable(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	queries := generatedQueries(t)
	if len(queries) < 100 {
		t.Fatalf("found %d generated queries; the source scan is broken", len(queries))
	}
	for name, query := range queries {
		for _, step := range queryPlan(t, raw, name, query) {
			match := planStep.FindStringSubmatch(step)
			if match == nil || !growingTables[match[1]] {
				continue
			}
			wholeTable := strings.HasPrefix(step, "SCAN ") && !strings.Contains(step, "INDEX")
			if !wholeTable && !strings.Contains(step, "AUTOMATIC") {
				continue
			}
			t.Errorf("%s scans all of %s (%q); scope it by conversation, session or branch", name, match[1], step)
		}
	}
}

// generatedQueries reads the query text sqlc wrote into this package, keyed by
// the query's sqlc name.
func generatedQueries(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	queries := map[string]string{}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Values) != 1 {
					continue
				}
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				header, _, found := strings.Cut(text, "\n")
				if !found || !strings.HasPrefix(header, "-- name: ") {
					continue
				}
				queries[strings.Fields(header)[2]] = text
			}
		}
	}
	return queries
}

func queryPlan(t *testing.T, db *sql.DB, name, query string) []string {
	t.Helper()
	// sqlc.slice placeholders expand at run time; one value plans the same way.
	query = strings.ReplaceAll(query, "/*SLICE:", "/*")
	args := make([]any, strings.Count(query, "?"))
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("plan %s: %v", name, err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	return plan
}

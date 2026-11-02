package sqlite

import (
	"database/sql"
	"testing"
)

// shippedForkMigrations are the fork migrations as Coding Tools 0.8.x applied
// them: at 0159-0164, which upstream later gave to other features.
var shippedForkMigrations = []legacyCodexProfileMigration{
	{version: 159, canonicalPath: "migrations/9001_session_plain_prompt.sql", legacyName: "session_plain_prompt.sql"},
	{version: 160, canonicalPath: "migrations/9002_session_updated_cdc_dedupe.sql", legacyName: "session_updated_cdc_dedupe.sql"},
	{version: 161, canonicalPath: "migrations/9003_provider_delta_marks.sql", legacyName: "provider_delta_marks.sql"},
	{version: 162, canonicalPath: "migrations/9004_sessions_live_preview_index.sql", legacyName: "sessions_live_preview_index.sql"},
	{version: 163, canonicalPath: "migrations/9005_session_updated_cdc_session_repeats.sql", legacyName: "session_updated_cdc_session_repeats.sql"},
	{version: 164, canonicalPath: "migrations/9006_open_work_by_session_indexes.sql", legacyName: "open_work_by_session_indexes.sql"},
}

func TestMigrateMovesShippedForkMigrationsToForkRange(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 158)
	applyLegacyCodexProfileMigrations(t, db, shippedForkMigrations)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate database with the fork's 0159-0164: %v", err)
	}

	assertAppliedMigrations(t, db, 159, 160, 161, 162, 163, 164, 165, 166, 167, 168, 169, 170)
	assertAppliedMigrations(t, db, 9001, 9002, 9003, 9004, 9005, 9006)
	// Upstream's 0159-0170 ran: automations (0161) and cues (0168) exist.
	for _, table := range []string{"automations", "cues", "conversation_provider_delta_marks"} {
		assertSchemaObject(t, db, "table", table)
	}
	assertSchemaObject(t, db, "index", "idx_sessions_live_preview")
	assertSchemaObject(t, db, "index", "idx_conversation_turns_open_by_session")
	assertSessionColumn(t, db, "plain_prompt")
	assertSessionColumn(t, db, "automation_run_id")
	var repeatsTrigger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'conversation_messages_cdc_insert' AND instr(sql, '- 512') > 0`).Scan(&repeatsTrigger); err != nil {
		t.Fatalf("read session-repeats trigger: %v", err)
	}
	if repeatsTrigger != 1 {
		t.Fatal("conversation_messages_cdc_insert lost the fork's session-repeats dedupe")
	}

	if err := migrate(db); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}
	var forkRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id BETWEEN 9001 AND 9006`).Scan(&forkRows); err != nil {
		t.Fatalf("count fork ledger rows: %v", err)
	}
	if forkRows != 6 {
		t.Fatalf("fork ledger rows = %d after a second pass, want 6", forkRows)
	}
}

func TestMigrateLeavesUpstreamVersionsWithoutForkSchemaAlone(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 170)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate upstream database: %v", err)
	}

	assertAppliedMigrations(t, db, 159, 160, 161, 162, 163, 164, 9001, 9002, 9003, 9004, 9005, 9006)
	var upstreamRows int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT version_id) FROM goose_db_version WHERE version_id BETWEEN 159 AND 164 AND is_applied = 1`).Scan(&upstreamRows); err != nil {
		t.Fatalf("count upstream ledger rows: %v", err)
	}
	if upstreamRows != 6 {
		t.Fatalf("upstream 0159-0164 ledger rows = %d, want 6", upstreamRows)
	}
}

func assertSchemaObject(t *testing.T, db *sql.DB, kind, name string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&count); err != nil {
		t.Fatalf("read %s %s: %v", kind, name, err)
	}
	if count != 1 {
		t.Fatalf("%s %s missing after migration", kind, name)
	}
}

func assertSessionColumn(t *testing.T, db *sql.DB, column string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, column).Scan(&count); err != nil {
		t.Fatalf("read sessions.%s: %v", column, err)
	}
	if count != 1 {
		t.Fatalf("sessions.%s missing after migration", column)
	}
}

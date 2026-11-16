package sqlite

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// archiveIndexBuild matches a statement that builds an index over one of the
// two tables that grow with every chat event.
var archiveIndexBuild = regexp.MustCompile(
	`(?is)\bcreate\s+(?:unique\s+)?index\b[^;]*?\bon\s+(conversation_provider_events|conversation_activities)\b`)

// archiveIndexMigrations are the shipped migrations that build such an index.
// Installs have run them; they stay as they are.
var archiveIndexMigrations = map[int64]bool{
	66: true, 74: true, 75: true, 77: true, 87: true, 107: true, 9006: true, 9009: true,
}

// A migration runs before AO listens. An index over the chat archives reads the
// whole table: with a cold file cache on a busy hard disk that is minutes in
// which AO answers nothing, the launcher kills an AO that is not ready within
// 15 minutes, the cut-off build rolls back and the next start begins it again
// (2026-11-16, after the update that brought 9009). A new index over these
// tables is built after startup instead, as Store.EnsureProviderChunkIndex
// builds the one migration 9010 used to.
func TestNoNewMigrationBuildsAnIndexOverTheChatArchives(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		version, err := goose.NumericComponent(e.Name())
		if err != nil || archiveIndexMigrations[version] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var statements []string
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				statements = append(statements, line)
			}
		}
		if match := archiveIndexBuild.FindStringSubmatch(strings.Join(statements, "\n")); match != nil {
			t.Errorf("migration %q builds an index over %s before AO listens: build it after startup, the way Store.EnsureProviderChunkIndex does",
				e.Name(), match[1])
		}
	}
}

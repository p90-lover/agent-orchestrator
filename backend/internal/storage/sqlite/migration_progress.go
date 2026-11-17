package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pressly/goose/v3"
)

// StageFunc is told each long step of Open as it begins: its name, and slog
// key-value pairs saying more.
type StageFunc func(name string, attrs ...any)

// OpenOption adjusts Open.
type OpenOption func(*openOptions)

type openOptions struct {
	stage StageFunc
}

// ReportStages has Open name its long steps through stage as they begin:
// "prepare migrations", "migrate" before each pending migration (with
// "migration", its file, and "left", how many remain with it), and
// "reconcile schema". A migration runs before the daemon listens and can take
// many minutes on a large database; the daemon reports these stages so its
// supervisor can tell that work from a hang (see internal/startupprogress).
func ReportStages(stage StageFunc) OpenOption {
	return func(options *openOptions) { options.stage = stage }
}

// migrationProgress tells stage which migration goose runs next. It is goose's
// logger while migrations run: goose prints "OK   <file> (<duration>)" (or
// "EMPTY ...") once a migration is applied, and the next pending one begins.
type migrationProgress struct {
	stage   StageFunc
	pending []string
	next    int
}

// newMigrationProgress lists the migrations goose.Up will apply, in its order:
// every embedded migration with no row in the ledger, ascending. With
// WithAllowMissing goose applies the missing ones (below the ledger's highest
// version) first and then the new ones (above it), which is the same order.
func newMigrationProgress(db *sql.DB, stage StageFunc) (*migrationProgress, error) {
	migrations, err := goose.CollectMigrations("migrations", 0, goose.MaxVersion)
	if err != nil {
		return nil, fmt.Errorf("collect migrations: %w", err)
	}
	known := map[int64]bool{}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version'`).Scan(&ledger); err != nil {
		return nil, err
	}
	if ledger > 0 {
		rows, err := db.Query(`SELECT DISTINCT version_id FROM goose_db_version`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var version int64
			if err := rows.Scan(&version); err != nil {
				return nil, err
			}
			known[version] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	progress := &migrationProgress{stage: stage}
	for _, migration := range migrations {
		if migration.Version > 0 && !known[migration.Version] {
			progress.pending = append(progress.pending, filepath.Base(migration.Source))
		}
	}
	return progress, nil
}

// announce reports the migration about to run, if one is left.
func (p *migrationProgress) announce() {
	if p.next < len(p.pending) {
		p.stage("migrate", "migration", p.pending[p.next], "left", len(p.pending)-p.next)
	}
}

func (p *migrationProgress) Printf(format string, v ...any) {
	fields := strings.Fields(fmt.Sprintf(format, v...))
	if len(fields) < 2 || (fields[0] != "OK" && fields[0] != "EMPTY") {
		return
	}
	for i := p.next; i < len(p.pending); i++ {
		if p.pending[i] == fields[1] {
			p.next = i + 1
			p.announce()
			return
		}
	}
}

// Fatalf matches goose.NopLogger: goose returns its errors as well.
func (p *migrationProgress) Fatalf(string, ...any) {}

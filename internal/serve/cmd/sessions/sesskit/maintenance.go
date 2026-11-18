package sesskit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/store/db/dbconn"
)

// compactHint prints the one-line pointer to `rush sessions compact` when
// a gc/purge run left enough free pages behind to be worth rebuilding for.
// Called from gc/purge only when they actually deleted something and are
// not in --dry-run/--json mode. Thresholds are package vars so tests can
// exercise the hint on multi-MB databases.
func CompactHint(ctx context.Context, conn *sql.DB, minBytes func() int64, minFraction func() float64) {
	stats, err := dbconn.Stats(ctx, conn)
	if err != nil {
		return
	}
	free := stats.FreeBytes
	if free < minBytes() {
		return
	}
	if float64(free) < minFraction()*float64(stats.Bytes) {
		return
	}
	fmt.Fprintf(os.Stderr, "session store: %s of %s is free pages; run 'rush sessions compact' to reclaim\n",
		HumanBytes(free), HumanBytes(stats.Bytes))
}

func HumanBytes(v int64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(v)/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(v)/(1<<20))
	case v >= 1024:
		return fmt.Sprintf("%.1f KB", float64(v)/1024)
	default:
		return fmt.Sprintf("%d B", v)
	}
}

func PercentOf(part, total int64) int {
	if total == 0 {
		return 0
	}
	return int(100 * part / total)
}

// auditPlaceholders renders "? , ? , ?" for an IN (...) clause of n ids.
func AuditPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// auditQueryChunk caps the IN (...) list of one statement. SQLite's variable
// limit is build-dependent (999 on old builds, 32766 on current ones), and a
// --all over thousands of sessions must not bet on either; 900 sits below the
// smallest of them.
const AuditQueryChunk = 900

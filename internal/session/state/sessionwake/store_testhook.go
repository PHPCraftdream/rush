package sessionwake

import (
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// QueriesForTest exposes the store's writer *db.Queries to external test
// packages (the wake-schedule fixture reads raw schedule rows the exported
// surface does not return). Test-only.
func (s *WakeScheduleStore) QueriesForTest() *db.Queries { return s.q }

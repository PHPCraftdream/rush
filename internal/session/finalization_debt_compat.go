package session

import (
	"database/sql"

	findebt "github.com/PHPCraftdream/rush/internal/session/batch/findebt"
)

type FinalizationDebtStore = findebt.FinalizationDebtStore

type AgentFinalizationDebtSettler = findebt.AgentFinalizationDebtSettler

func NewFinalizationDebtStore(sqlDB *sql.DB) *FinalizationDebtStore {
	return findebt.NewFinalizationDebtStore(sqlDB)
}

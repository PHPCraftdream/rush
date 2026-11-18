package session

import (
	finholds "github.com/PHPCraftdream/rush/internal/session/batch/finholds"
)

var ErrFinalizationPhysicalHoldsUnreadable = finholds.ErrFinalizationPhysicalHoldsUnreadable

func NewFinalizationPhysicalHoldsFacts() FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFacts()
}

func NewFinalizationHostDriverHoldsFacts(dataDir string) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationHostDriverHoldsFacts(dataDir)
}

type FinalizationHoldsProvider = finholds.FinalizationHoldsProvider

func NewFinalizationPhysicalHoldsFactsFromProviders(providers ...FinalizationHoldsProvider) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFactsFromProviders(providers...)
}

func NewFinalizationPhysicalHoldsFactsSum(a, b FinalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFactsSum(a, b)
}

func CountFinalizationTrackedProcessHolds() int {
	return finholds.CountFinalizationTrackedProcessHolds()
}

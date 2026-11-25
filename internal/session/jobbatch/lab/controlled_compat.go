package lab

import "github.com/PHPCraftdream/rush/internal/session/jobbatch/lab/controlled"

func NewControlledExecutor() *controlled.ControlledExecutor {
	return controlled.NewControlledExecutor()
}

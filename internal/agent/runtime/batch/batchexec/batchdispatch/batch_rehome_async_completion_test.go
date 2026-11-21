package batchdispatch_test

import (
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
)

func TestAsyncCompletionCLIMarkerPrivate(t *testing.T) {
	field, ok := reflect.TypeOf(batchdispatch.AsyncCompletion{}).FieldByName("cli")
	if !ok || field.IsExported() || field.Type.Kind() != reflect.Bool {
		t.Fatal("completion CLI marker must remain a private bool")
	}
}

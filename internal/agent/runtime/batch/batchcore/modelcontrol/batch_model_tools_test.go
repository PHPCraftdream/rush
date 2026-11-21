package modelcontrol

import (
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

func TestBuildSpecCarriesPathAndInheritedDepth(t *testing.T) {
	rootCap, nestedCap := 3, 1
	raw := Spec{Mode: "parallel", MaxAgentDepth: &rootCap, Items: []Item{
		{Tool: "one"},
		{Batch: &Spec{Mode: "sequential", MaxAgentDepth: &nestedCap, Items: []Item{{Tool: "two"}}}},
	}}
	type leaf struct {
		tool, path string
		depth      int
	}
	var leaves []leaf
	spec, err := BuildSpec(raw, 4, SpecDeps{
		HasRegistry: true, MaxDepth: 8, MaxNodes: 16,
		Measure: func([]jobbatch.Spec) (int, int, error) { return 1, 1, nil },
		BuildLeaf: func(item Item, path string, depth int) (*jobbatch.TaskSpec, error) {
			leaves = append(leaves, leaf{item.Tool, path, depth})
			return &jobbatch.TaskSpec{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []leaf{{"one", "0", 3}, {"two", "1.0", 1}}; !reflect.DeepEqual(leaves, want) {
		t.Fatalf("leaf ports received %v, want %v", leaves, want)
	}
	if spec.Batch.Notify != jobbatch.NotifyAll || spec.Batch.Items[1].Batch.OnFail != jobbatch.OnFailStop {
		t.Fatalf("batch defaults not applied: %+v", spec.Batch)
	}
}

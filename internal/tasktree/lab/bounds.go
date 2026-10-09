package lab

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

// LAB-only transport/report bounds keep local probes manageable, independent of tree limits.
const (
	MaxInputBytes        = 4 * 1024 * 1024 // Includes whitespace; enough for modest checkpoint fixtures.
	MaxScenarioSteps     = 512             // Bounds execution and retained per-step metadata.
	MaxRetainedTextBytes = 256 * 1024      // Bounds repeated rendered output, not a single tree's labels.
	MaxRetainedNodes     = 4096            // Counts occurrences, so repeated views consume the budget again.
	MaxJSONDepth         = 128             // Bounds scanner state and subsequent typed decoding stack depth.
)

type honestWriter struct{ writer io.Writer }

func (w honestWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type boundedCheckpointWriter struct {
	writer    io.Writer
	remaining int
}

func (w *boundedCheckpointWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("lab checkpoint exceeds %d encoded bytes", MaxInputBytes)
	}
	n, err := (honestWriter{writer: w.writer}).Write(p)
	if n > 0 && n <= len(p) {
		w.remaining -= n
	}
	return n, err
}

type inputCounter struct{ remaining int }

func (w *inputCounter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("lab scenario exceeds %d encoded bytes", MaxInputBytes)
	}
	w.remaining -= len(p)
	return len(p), nil
}

// Encode one step at a time rather than allocating a second full scenario JSON copy.
func checkScenarioSize(scenario Scenario) error {
	counter := &inputCounter{remaining: MaxInputBytes}
	header := scenario
	header.Steps = nil
	if err := json.NewEncoder(counter).Encode(header); err != nil {
		return err
	}
	// Omit the newline; a nonnil steps slice replaces null with [].
	counter.remaining++
	if scenario.Steps != nil {
		counter.remaining += 2
	}
	for i, step := range scenario.Steps {
		if len(step.Payload) > MaxInputBytes {
			return fmt.Errorf("lab payload exceeds input bound")
		}
		if i > 0 {
			if _, err := counter.Write([]byte{','}); err != nil {
				return err
			}
		}
		if !json.Valid(step.Payload) {
			// Direct probes may intentionally exercise correctable protocol syntax errors.
			if len(step.Payload) > 4 {
				if len(step.Payload)-4 > counter.remaining {
					return fmt.Errorf("lab payload exceeds input bound")
				}
				counter.remaining -= len(step.Payload) - 4
			}
			step.Payload = json.RawMessage("null")
		}
		// The newline is not part of the enclosing array.
		counter.remaining++
		if err := json.NewEncoder(counter).Encode(step); err != nil {
			return err
		}
	}
	return nil
}

type reportBudget struct{ text, nodes int }

func (b *reportBudget) retain(result protocol.ToolResult) error {
	if len(result.Text) > MaxRetainedTextBytes-b.text {
		return fmt.Errorf("lab retained Text exceeds %d bytes", MaxRetainedTextBytes)
	}
	remaining := MaxRetainedNodes - b.nodes
	if len(result.Details.Created) > remaining {
		return fmt.Errorf("lab retained nodes exceed %d occurrences", MaxRetainedNodes)
	}
	remaining -= len(result.Details.Created)
	type cursor struct {
		nodes []tasktree.NodeView
		next  int
	}
	var stack []cursor
	if result.Details.View != nil {
		stack = append(stack, cursor{nodes: []tasktree.NodeView{result.Details.View.Root}})
	}
	for len(stack) > 0 {
		frame := &stack[len(stack)-1]
		if frame.next == len(frame.nodes) {
			stack = stack[:len(stack)-1]
			continue
		}
		if remaining == 0 {
			return fmt.Errorf("lab retained nodes exceed %d occurrences", MaxRetainedNodes)
		}
		remaining--
		node := &frame.nodes[frame.next]
		frame.next++
		if len(node.Children) > 0 {
			stack = append(stack, cursor{nodes: node.Children})
		}
	}
	b.text += len(result.Text)
	b.nodes = MaxRetainedNodes - remaining
	return nil
}

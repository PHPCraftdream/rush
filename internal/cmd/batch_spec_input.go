package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/spf13/cobra"
)

type batchSpecInput struct {
	Mode        string           `json:"mode"`
	OnFail      string           `json:"on_fail,omitempty"`
	Notify      string           `json:"notify,omitempty"`
	MaxParallel int              `json:"max_parallel,omitempty"`
	Items       []batchSpecInput `json:"items,omitempty"`
	Tool        string           `json:"tool,omitempty"`
	Input       json.RawMessage  `json:"input,omitempty"`
}

func (x batchSpecInput) spec() (jobbatch.Spec, error) {
	if x.Tool != "" || x.Input != nil {
		if x.Tool == "" || x.Mode != "" || x.OnFail != "" || x.Notify != "" || x.MaxParallel != 0 || len(x.Items) != 0 {
			return jobbatch.Spec{}, errors.New("task and batch fields cannot be mixed")
		}
		raw, err := json.Marshal(agent.BatchCLITask{Tool: x.Tool, Input: x.Input})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: raw}}, nil
	}
	if x.Mode == "" {
		x.Mode = string(jobbatch.ModeParallel)
	}
	if x.OnFail == "" && x.Mode == string(jobbatch.ModeSequential) {
		x.OnFail = string(jobbatch.OnFailStop)
	}
	if x.Notify == "" {
		x.Notify = string(jobbatch.NotifyAll)
	}
	b := &jobbatch.BatchSpec{Mode: jobbatch.Mode(x.Mode), OnFail: jobbatch.OnFail(x.OnFail), Notify: jobbatch.Notify(x.Notify), MaxParallel: x.MaxParallel}
	if b.Mode == jobbatch.ModeParallel && b.OnFail != "" {
		return jobbatch.Spec{}, errors.New("on_fail is only valid for sequential batches")
	}
	for _, child := range x.Items {
		item, err := child.spec()
		if err != nil {
			return jobbatch.Spec{}, err
		}
		b.Items = append(b.Items, item)
	}
	return jobbatch.Spec{Batch: b}, nil
}

func readBatchSpec(cmd *cobra.Command) ([]byte, error) {
	name, _ := cmd.Flags().GetString("spec")
	if name == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(name)
}

func looksLikeKtav(raw []byte) bool {
	// Only select Ktav when the entire stdin payload is not a valid JSON
	// document. Prefix-based guesses misclassify JSON comments/strings and
	// quoted object keys as Ktav, while valid JSON must remain JSON even if
	// its root later fails the batch schema.
	_, err := ktavcodec.ParseJSONLossless(raw)
	return err != nil
}

// startsWithJSONValue reports whether raw opens with one complete JSON value.
func startsWithJSONValue(raw []byte) bool {
	var v any
	return json.NewDecoder(bytes.NewReader(raw)).Decode(&v) == nil
}

func parseBatchSpec(cmd *cobra.Command) (jobbatch.Spec, error) {
	raw, err := readBatchSpec(cmd)
	if err != nil {
		return jobbatch.Spec{}, err
	}
	format := "json"
	name, _ := cmd.Flags().GetString("spec")
	if name != "-" {
		if strings.EqualFold(filepath.Ext(name), ".ktav") {
			format = "ktav"
		}
	} else if looksLikeKtav(raw) {
		format = "ktav"
	}
	if format == "ktav" {
		tree, decodeErr := ktavcodec.DecodeKtav(raw)
		if decodeErr != nil {
			if name == "-" && startsWithJSONValue(raw) {
				// JSON followed by more input: the JSON diagnosis is the useful one.
				return jobbatch.Spec{}, errors.New("trailing JSON value")
			}
			return jobbatch.Spec{}, decodeErr
		}
		raw, err = ktavcodec.ToJSON(tree)
		if err != nil {
			return jobbatch.Spec{}, err
		}
	}
	var input batchSpecInput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&input); err != nil {
		return jobbatch.Spec{}, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return jobbatch.Spec{}, err
	}
	return input.spec()
}

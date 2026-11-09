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
	spec, _, err := parseBatchSpecWithDepth(cmd)
	return spec, err
}

func parseBatchSpecWithDepth(cmd *cobra.Command) (jobbatch.Spec, int, error) {
	raw, err := readBatchSpec(cmd)
	if err != nil {
		return jobbatch.Spec{}, 0, err
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
				return jobbatch.Spec{}, 0, errors.New("trailing JSON value")
			}
			return jobbatch.Spec{}, 0, decodeErr
		}
		raw, err = ktavcodec.ToJSON(tree)
		if err != nil {
			return jobbatch.Spec{}, 0, err
		}
	}
	spec, err := agent.ParseBatchExternalSpecJSON(raw)
	if err != nil {
		return jobbatch.Spec{}, 0, err
	}
	var root struct {
		MaxAgentDepth *int `json:"max_agent_depth"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return jobbatch.Spec{}, 0, err
	}
	depth := agent.DefaultAgentDepth
	if root.MaxAgentDepth != nil {
		depth = *root.MaxAgentDepth
	}
	return spec, depth, nil
}

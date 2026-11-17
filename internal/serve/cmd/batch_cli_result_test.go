package cmd

// batchCLIResult is a byte-identical local copy of the type in
// cli/batch_cli_test.go: the batch e2e test stays in package cmd.
type batchCLIResult struct {
	Output           string
	Error            string
	RunIncompleteErr bool
}

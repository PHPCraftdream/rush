package queuecmd

// batchCLIResult is a byte-identical local copy of the type in
// cli/batchclitest/batch_cli_test.go.
type batchCLIResult struct {
	Output           string
	Error            string
	RunIncompleteErr bool
}

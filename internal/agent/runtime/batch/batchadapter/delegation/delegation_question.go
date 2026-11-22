package delegation

import (
	"strings"
)

const childQuestionMaxLen = 300

// TrimChildQuestion preserves the original byte cap and whitespace trim semantics.
func TrimChildQuestion(question string) string {
	if len(question) > childQuestionMaxLen {
		return strings.TrimSpace(question[:childQuestionMaxLen]) + "..."
	}
	return question
}

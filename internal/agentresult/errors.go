package agentresult

import "encoding/json"

type Code string

const (
	CodeInvalid  Code = "output_contract_invalid"
	CodeMismatch Code = "output_contract_mismatch"
	CodeTooLarge Code = "output_too_large"
)

type Error struct {
	Code        Code
	Diagnostics []Diagnostic
}

func (e *Error) Error() string {
	if e == nil || !knownContractCode(e.Code) {
		return string(CodeInvalid)
	}
	return string(e.Code)
}

func (e *Error) ContractCode() Code {
	if e == nil || !knownContractCode(e.Code) {
		return CodeInvalid
	}
	return e.Code
}

func (e *Error) Feedback(attempt, remaining int) string {
	if e == nil {
		return (&Error{Code: CodeInvalid}).Feedback(attempt, remaining)
	}
	return feedback(e.ContractCode(), e.Diagnostics, attempt, remaining)
}

func knownContractCode(code Code) bool {
	return code == CodeInvalid || code == CodeMismatch || code == CodeTooLarge
}

func feedback(code Code, diagnostics []Diagnostic, attempt, remaining int) string {
	if !knownContractCode(code) {
		code = CodeInvalid
	}
	if attempt < 0 {
		attempt = 0
	}
	if remaining < 0 {
		remaining = 0
	}
	// Диагностики формируются только из фиксированных безопасных значений; указатели и пользовательский текст исключены.
	safe := make([]Diagnostic, 0, 8)
	for i, d := range diagnostics {
		if i >= 8 {
			break
		}
		reason, ok := safeDiagnosticReason(d.Code)
		if !ok {
			continue
		}
		safe = append(safe, Diagnostic{Code: d.Code, InstancePointer: "", SchemaPointer: "", Reason: reason})
	}
	payload := struct {
		Code        Code         `json:"code"`
		Attempt     int          `json:"attempt"`
		Remaining   int          `json:"remaining"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}{code, attempt, remaining, safe}
	b, _ := json.Marshal(payload)
	if len(b) <= 4096 {
		return string(b)
	}
	payload.Diagnostics = nil
	b, _ = json.Marshal(payload)
	if len(b) <= 4096 {
		return string(b)
	}
	return `{"code":"output_contract_invalid","attempt":0,"remaining":0,"diagnostics":[]}`
}

func safeDiagnosticReason(code string) (string, bool) {
	switch code {
	case "number_not_representable":
		return "number is not exactly representable", true
	case "schema_mismatch":
		return "candidate does not satisfy schema", true
	case "regex_mismatch":
		return "candidate does not match pattern", true
	case "invalid_json":
		return "candidate is not valid JSON", true
	default:
		return "", false
	}
}

func (e *Error) As(target any) bool {
	p, ok := target.(**ValidationError)
	if !ok || e == nil || e.Code != CodeMismatch {
		return false
	}
	*p = &ValidationError{Diagnostics: e.Diagnostics}
	return true
}

func (e *ValidationError) ContractCode() Code { return CodeMismatch }

func (e *ValidationError) Feedback(attempt, remaining int) string {
	if e == nil {
		return (&Error{Code: CodeMismatch}).Feedback(attempt, remaining)
	}
	return feedback(CodeMismatch, e.Diagnostics, attempt, remaining)
}

func (e *ValidationError) As(target any) bool {
	p, ok := target.(**Error)
	if !ok {
		return false
	}
	*p = &Error{Code: CodeMismatch, Diagnostics: e.Diagnostics}
	return true
}

// Package agentresult реализует ограниченный профиль JSON Schema 2020-12.
// DecodeSpec проверяет входную спецификацию и компилирует её на этапе admission;
// Compile отдельно компилирует программную Spec для конкретного вызова, а
// Compiled.Validate проверяет полный текст кандидата. Диагностики schema mismatch
// намеренно ограничены root-only и не отражают secret keys. Регулярные выражения
// используют Go RE2, а не ECMAScript. Числа проверяются через десятичный
// round-trip float64 и safe-integer предел; полный кандидат ограничен 1 MiB.
// Пакет не обращается к сети.
package agentresult

package agent

import (
	"errors"
	"net/http"
	"regexp"

	"charm.land/fantasy"
)

// The tail of a wake_failed marker: what the operator can expect next.
const (
	// settleTailNextTurn: the events are in history and a later turn may react.
	settleTailNextTurn = "Событие сохранено; продолжение — при следующем ходе."
	// settleTailContextTooLarge: the events are in history and the history no
	// longer fits, so every next request of the session fails the same way until
	// it is shrunk (R8B-5).
	settleTailContextTooLarge = "Событие сохранено в истории, но она уже не помещается в окно контекста модели: следующие запросы этой сессии тоже будут отклонены. Сожмите историю (summarize) или начните новую сессию."
)

// contextOverflowMessage matches the providers that report an oversized prompt
// as a plain 400 (fantasy flags only the Anthropic and Google shapes).
var contextOverflowMessage = regexp.MustCompile(`(?i)prompt is too long|input is too long|context[_ ]length|maximum context|context window|too many tokens|reduce the length`)

// isContextOverflow reports whether err says the request did not fit the
// model's context window: a flagged fantasy error, a 413, or a 400 whose
// message names the overflow.
func isContextOverflow(err error) bool {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	if providerErr.IsContextTooLarge() || providerErr.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	return providerErr.StatusCode == http.StatusBadRequest && contextOverflowMessage.MatchString(providerErr.Message)
}

// settleTailFor picks the marker tail for the failure that closed the debt.
func settleTailFor(err error) string {
	if isContextOverflow(err) {
		return settleTailContextTooLarge
	}
	return settleTailNextTurn
}

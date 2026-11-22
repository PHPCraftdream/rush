package subagents

import "context"

func DelegatedChildDriven(
	ctx context.Context,
	sessionID string,
	parentFor func(context.Context, string) (string, error),
	running func(context.Context, string, string) (bool, error),
	registered func(string) bool,
) (bool, error) {
	parent, err := parentFor(ctx, sessionID)
	if err != nil {
		return registered(sessionID), err
	}
	if parent != "" {
		driven, err := running(ctx, parent, sessionID)
		if err == nil {
			return driven, nil
		}
		return registered(sessionID), err
	}
	return registered(sessionID), nil
}

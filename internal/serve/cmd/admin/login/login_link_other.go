//go:build !windows

package login

import "context"

func startPhysicalOAuthLinkControls(context.Context, *oauthLinkModel) (func(), bool) {
	return nil, false
}

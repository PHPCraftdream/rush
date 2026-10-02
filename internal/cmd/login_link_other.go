//go:build !windows

package cmd

import "context"

func startPhysicalOAuthLinkControls(context.Context, *oauthLinkModel) (func(), bool) {
	return nil, false
}

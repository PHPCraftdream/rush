//go:build windows

package config

func configParentIdentity(path string) configFileIdentity {
	parent, err := openWindowsConfigParent(path)
	if err != nil {
		return configFileIdentity{}
	}
	defer parent.Close()
	return configFileIdentityOfOpened(parent, nil)
}

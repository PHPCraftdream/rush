//go:build windows

package config

func configParentIdentity(path string) configFileIdentity {
	physicalPath, err := physicalWindowsConfigPath(path)
	if err != nil {
		return configFileIdentity{}
	}
	parent, err := openWindowsConfigParent(physicalPath)
	if err != nil {
		return configFileIdentity{}
	}
	defer parent.Close()
	return configFileIdentityOfOpened(parent, nil)
}

package config

import "github.com/PHPCraftdream/rush/internal/config/store/settings"

func init() { _ = settings.SetSettingsPasswordIterationsForTest(1000) }

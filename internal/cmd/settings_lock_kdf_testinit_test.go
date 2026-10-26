package cmd

import "github.com/PHPCraftdream/rush/internal/config"

// Lower the PBKDF2 cost for the whole cmd test binary: config.HashPassword
// is called here only in test fixtures, and production iterations (~600k)
// would make seeding and refusal-path runs needlessly slow.
func init() { _ = config.SetSettingsPasswordIterationsForTest(1000) }

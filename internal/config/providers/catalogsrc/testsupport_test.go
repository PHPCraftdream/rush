package catalogsrc

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
)

func ktavSiblingOf(path string) string { return docread.KtavSiblingOf(path) }

func ktavEntryExists(path string) bool { return docread.KtavEntryExists(path) }

// test-only forwarder; io layer: slots.
func setKtavAutoMigrateForTest(on bool) (restore func()) {
	return slots.SetKtavAutoMigrateForTest(on)
}

// test-only forwarder; io layer: cfgcommit.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return cfgcommit.AtomicWriteFile(path, data, perm)
}

// test-only forwarder; io layer: slots.
func readCacheDocument(legacyJSON string) ([]byte, error) { return slots.ReadCacheDocument(legacyJSON) }

// test-only forwarder; io layer: slots.
func writeCacheDocument(legacyJSON string, jsonBytes []byte, perm os.FileMode, writeJSON func(path string, data []byte, perm os.FileMode) error) error {
	return slots.WriteCacheDocument(legacyJSON, jsonBytes, perm, writeJSON)
}

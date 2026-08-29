// Package negativeimport is a compile-time negative fixture: it imports
// rush's internal tree from OUTSIDE the rush module, which the Go
// internal/ rule must reject. Pattern commands never build it (testdata
// directories are skipped during package walks); embed_test.go's
// TestExternalModuleCannotImportInternal builds it explicitly and
// requires the build to fail.
package negativeimport

import _ "github.com/PHPCraftdream/rush/internal/app"

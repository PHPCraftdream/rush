package verifystatic

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryGuardRatchetGrowthAndCleanup(t *testing.T) {
	all := map[string]sourceFile{}
	for i := 0; i < 10; i++ {
		all[fmt.Sprintf("big/%c.go", 'a'+i)] = sourceFile{}
	}
	if r := directoryGuard("dir-entries", all, map[string]int{"big": 9}, 7, false); r.Status != "fail" {
		t.Fatalf("growth passed: %+v", r)
	}
	if r := directoryGuard("dir-entries", all, map[string]int{"big": 10}, 7, false); r.Status != "pass" {
		t.Fatalf("unchanged debt failed: %+v", r)
	}
	clean := map[string]sourceFile{}
	for i := 0; i < 7; i++ {
		clean[fmt.Sprintf("big/%c.go", 'a'+i)] = sourceFile{}
	}
	if r := directoryGuard("dir-entries", clean, map[string]int{"big": 7}, 7, false); r.Status != "fail" {
		t.Fatal("allowlisted drop-to-limit passed")
	}
}

func TestDirectoryEntriesSelftestSemantics(t *testing.T) {
	all := map[string]sourceFile{}
	for i := 0; i < 7; i++ {
		all[fmt.Sprintf("src/f%d.go", i)] = sourceFile{}
	}
	for i := 0; i < 12; i++ {
		all[fmt.Sprintf("src/f%d_test.go", i)] = sourceFile{}
	}
	if r := directoryGuard("dir-entries", all, map[string]int{}, 7, false); r.Status != "pass" {
		t.Fatalf("tests counted by default: %+v", r)
	}
	if r := directoryGuard("dir-entries", all, map[string]int{}, 7, true); r.Status != "fail" {
		t.Fatal("DIR_ENTRIES_COUNT_TESTS equivalent failed to count tests")
	}
	if r := directoryGuard("dir-entries", all, map[string]int{}, 7, true); r.Files != 19 {
		t.Fatalf("wrong test-inclusive count %d", r.Files)
	}
}

func TestNestedDirectoryAndExtensions(t *testing.T) {
	all := map[string]sourceFile{}
	for i := 0; i < 6; i++ {
		all[fmt.Sprintf("parent/%d.go", i)] = sourceFile{}
	}
	for i := 0; i < 7; i++ {
		all[fmt.Sprintf("parent/child/%d.tsx", i)] = sourceFile{}
	}
	if r := directoryGuard("dir-entries", all, map[string]int{}, 7, false); r.Status != "pass" {
		t.Fatalf("six files plus child directory should pass: %+v", r)
	}
	all["parent/extra.ts"] = sourceFile{}
	if r := directoryGuard("dir-entries", all, map[string]int{}, 7, false); r.Status != "fail" {
		t.Fatal("eight parent entries passed")
	}
	if r := directoryGuard("dir-files", all, map[string]int{}, 21, true); r.Status != "pass" {
		t.Fatal("non-Go source counted incorrectly")
	}
}

func TestFileLengthYMLBijectionRemainsShellOnly(t *testing.T) {
	t.Skip(".golangci.yml file-length-limit and allowlist bijection is verified only by .githooks/check_file_size_selftest.sh")
}

func TestFileSizeSelftestBoundariesAndRatchets(t *testing.T) {
	files := map[string]sourceFile{"big.go": {lines: 1001}, "gen.go": {lines: 1200, generated: true}, "ok.ts": {lines: 1000}}
	warnings := map[string]sourceFile{"warn1.go": {lines: 901}, "warn2.go": {lines: 950}, "gen.go": {lines: 1200, generated: true}}
	warningResult := sizeGuard("file-size", warnings, map[string]int{}, 1000)
	if warningResult.Status != "pass" || !strings.Contains(warningResult.Summary, "2 hand-written file(s) over 900 lines (warning, limit 1000)") || !strings.Contains(warningResult.Summary, "950 warn2.go") || !strings.Contains(warningResult.Summary, "901 warn1.go") || strings.Contains(warningResult.Summary, "gen.go") || strings.Index(warningResult.Summary, "950 warn2.go") > strings.Index(warningResult.Summary, "901 warn1.go") {
		t.Fatalf("warning-band presentation mismatch: %+v", warningResult)
	}
	topFive := map[string]sourceFile{}
	for p, n := range map[string]int{"one": 999, "two": 998, "three": 997, "four": 996, "five": 995, "six": 994} {
		topFive[p] = sourceFile{lines: n}
	}
	top := sizeGuard("file-size", topFive, map[string]int{}, 1000)
	if strings.Contains(top.Summary, "six") || !strings.Contains(top.Summary, "995 five") {
		t.Fatalf("top-five ordering/presentation wrong: %s", top.Summary)
	}

	if r := sizeGuard("file-size", files, map[string]int{}, 1000); r.Status != "fail" {
		t.Fatal("1001 lines passed")
	}
	if r := sizeGuard("file-size", map[string]sourceFile{"ok.go": {lines: 1000}}, map[string]int{}, 1000); r.Status != "pass" {
		t.Fatal("exact 1000 failed")
	}
	if r := sizeGuard("file-size", map[string]sourceFile{"big.go": {lines: 1100}}, map[string]int{"big.go": 1100}, 1000); r.Status != "pass" {
		t.Fatal("allowlisted exact count failed")
	}
	if r := sizeGuard("file-size", map[string]sourceFile{"big.go": {lines: 1101}}, map[string]int{"big.go": 1100}, 1000); r.Status != "fail" {
		t.Fatal("allowlist growth passed")
	}
	if r := sizeGuard("file-size", map[string]sourceFile{"big.go": {lines: 999}}, map[string]int{"big.go": 1100}, 1000); r.Status != "fail" {
		t.Fatal("drop-to-limit passed")
	}
	if r := sizeGuard("file-size-900", map[string]sourceFile{"big.ts": {lines: 901}}, map[string]int{}, 900); r.Status != "fail" {
		t.Fatal("901 lines passed")
	}
	if r := sizeGuard("file-size-900", map[string]sourceFile{"ok.ts": {lines: 900}}, map[string]int{}, 900); r.Status != "pass" {
		t.Fatal("900 line boundary failed")
	}
	if r := sizeGuard("file-size", map[string]sourceFile{"ok.go": {lines: 1000}}, map[string]int{}, 1000); r.Status != "pass" {
		t.Fatal("1000 line boundary failed")
	}
	if r := sizeGuard("file-size-900", map[string]sourceFile{"gen.go": {lines: 1200, generated: true}}, map[string]int{}, 900); r.Status != "pass" {
		t.Fatal("generated file failed")
	}
}

func TestStrictAllowlistCounts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt"} {
		if err := os.WriteFile(filepath.Join(root, ".githooks", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".githooks", "dir_entries_allowlist.txt"), []byte("src +7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAllow(root, "dir_entries_allowlist.txt"); err == nil || !strings.Contains(err.Error(), "invalid count") {
		t.Fatalf("existing malformed allowlist should report invalid count, got %v", err)
	}
}

func TestDirectoryGuardEmptyInputAndRootOnlyCounts(t *testing.T) {
	rootOnly := map[string]sourceFile{"root.go": {}}
	if r := directoryGuard("dir-entries", rootOnly, map[string]int{}, 7, false); r.Directories != 0 || r.Files != 0 {
		t.Fatalf("root entries must remain exempt for anti-vacuous classification: %+v", r)
	}
	if r := directoryGuard("dir-files", rootOnly, map[string]int{}, 21, true); r.Directories != 0 || r.Files != 0 {
		t.Fatalf("root files must remain exempt for anti-vacuous classification: %+v", r)
	}
	filesOnly := map[string]sourceFile{"src/testdata/x.go": {}, "src/dist/y.ts": {}, "src/node_modules/z.tsx": {}}
	if r := directoryGuard("dir-entries", filesOnly, map[string]int{}, 7, false); r.Directories != 0 || r.Files != 0 {
		t.Fatalf("ignored-only inputs should examine zero applicable paths: %+v", r)
	}
	if r := directoryGuard("dir-files", filesOnly, map[string]int{}, 21, true); r.Directories != 0 || r.Files != 0 {
		t.Fatalf("ignored-only inputs should examine zero applicable paths: %+v", r)
	}
}

func TestTestdataAndGeneratedWindowAndDirectoryCounts(t *testing.T) {
	all := map[string]sourceFile{"src/a.go": {}, "src/b.go": {}, "src/testdata/x.go": {}, "src/node_modules/y.ts": {}, "src/dist/z.tsx": {}, "src/gen.go": {generated: true}, "root.go": {}}
	entries := directoryGuard("dir-entries", all, map[string]int{}, 7, false)
	if entries.Status != "pass" || entries.Files != 2 || entries.Directories != 1 {
		t.Fatalf("ignored paths/root/generated semantics: %+v", entries)
	}
	child := map[string]sourceFile{}
	for i := 0; i < 6; i++ {
		child[fmt.Sprintf("parent/f%d.go", i)] = sourceFile{}
	}
	child["parent/child/x.go"] = sourceFile{}
	if r := directoryGuard("dir-entries", child, map[string]int{}, 7, false); r.Status != "pass" || r.Files != 7 {
		t.Fatalf("six files + one child dir must pass: %+v", r)
	}
	for i := 0; i < 7; i++ {
		child[fmt.Sprintf("parent/g%d.go", i)] = sourceFile{}
	}
	if r := directoryGuard("dir-entries", child, map[string]int{}, 7, false); r.Status != "fail" {
		t.Fatalf("seven files + child directory must fail: %+v", r)
	}
	if r := directoryGuard("dir-files", map[string]sourceFile{"src/a.go": {}, "src/a_test.go": {}, "src/b.ts": {}, "src/c.tsx": {}}, map[string]int{}, 21, true); r.Files != 4 || r.Status != "pass" {
		t.Fatalf("dir-files must count tests and mixed TS/TSX files: %+v", r)
	}
	boundary := make(map[string]sourceFile, 21)
	for i := 0; i < 21; i++ {
		boundary[fmt.Sprintf("src/f%02d.go", i)] = sourceFile{}
	}
	if r := directoryGuard("dir-files", boundary, map[string]int{}, 21, true); r.Status != "pass" || r.Files != 21 {
		t.Fatalf("21 file boundary: %+v", r)
	}
}

func TestGeneratedDetectionOnlyFirstTenLinesAndStaleSizeRatchet(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt", "layer_exceptions.txt", "layer_test_exceptions.txt", "pkg_names_allowlist.txt"} {
		if err := os.WriteFile(filepath.Join(root, ".githooks", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	firstBytes := []byte("// Code generated; DO NOT EDIT.\npackage fixture\n" + strings.Repeat("x\n", 999))
	lateBytes := []byte("package fixture\n" + strings.Repeat("x\n", 10) + "// Code generated; DO NOT EDIT.\n" + strings.Repeat("x\n", 989))
	if err := os.WriteFile(filepath.Join(root, "first.go"), firstBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "late.go"), lateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// Restored slice-1 coverage: the fixtures are real files on disk and
	// the generated detection is exercised end to end through Run (the
	// toolchain-backed guards get a canned go list / empty git grep).
	results, err := Run(root, []string{"first.go", "late.go"}, false, func(cwd string, env []string, args ...string) ([]byte, error) {
		_ = cwd
		_ = env
		if args[0] == "go" && args[len(args)-1] == "-m" {
			return []byte("fixture\n"), nil
		}
		if args[0] == "go" {
			return []byte("{\"ImportPath\":\"fixture\"}\n"), nil
		}
		// git grep exit 1: zero db candidates, a legitimate pass.
		return nil, cannedExit{code: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	// results[2] is file-size, results[3] is file-size-900 (slice 1's
	// indexing; the earlier two-element-slice results[3] bug is gone).
	if results[2].Status != "fail" || !strings.Contains(results[2].Excerpt, "late.go") {
		t.Fatalf("marker after ten lines must not exempt source: %+v", results[2])
	}
	if results[2].Files != 2 || !strings.Contains(results[2].Summary, "1 generated skipped") {
		t.Fatalf("first-window generated file not skipped or counts wrong: %+v", results[2])
	}
	if results[3].Files != 2 || !strings.Contains(results[3].Summary, "1 generated skipped") || results[3].Status != "fail" {
		t.Fatalf("900-line guard generated/examined semantics wrong: %+v", results[3])
	}
	if r := sizeGuard("file-size-900", map[string]sourceFile{"src/gone.ts": {lines: 0}}, map[string]int{"src/gone.ts": 950}, 900); r.Status != "fail" {
		t.Fatal("stale/fixed size ratchet entry passed")
	}
}

func TestAllowlistCommentsBlankAndDuplicateEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt"} {
		if err := os.WriteFile(filepath.Join(root, ".githooks", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(root, ".githooks", "dir_entries_allowlist.txt")
	if err := os.WriteFile(p, []byte("# comment\n\nsrc    8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readAllow(root, "dir_entries_allowlist.txt"); err != nil || got["src"] != 8 {
		t.Fatalf("comments, blank lines, or repeated path/count whitespace rejected: %#v %v", got, err)
	}
	if err := os.WriteFile(p, []byte("src    8\n\n# whitespace-normalized duplicate follows\nsrc 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAllow(root, "dir_entries_allowlist.txt"); err == nil {
		t.Fatal("duplicate allowlist entry accepted")
	}
}

func TestStaleAllowlistEntriesDoNotFail(t *testing.T) {
	// Restored slice-1 coverage: real files on disk through Run.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt", "layer_exceptions.txt", "layer_test_exceptions.txt", "pkg_names_allowlist.txt"} {
		content := ""
		if name == "file_size_900_allowlist.txt" {
			content = "gone.ts 950\n"
		}
		if err := os.WriteFile(filepath.Join(root, ".githooks", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "ok.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := Run(root, []string{"src/ok.go"}, false, func(cwd string, env []string, args ...string) ([]byte, error) {
		_ = cwd
		_ = env
		if args[0] == "go" && args[len(args)-1] == "-m" {
			return []byte("fixture\n"), nil
		}
		if args[0] == "go" {
			return []byte("{\"ImportPath\":\"fixture/src\"}\n"), nil
		}
		return nil, cannedExit{code: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results[:4] {
		if r.Status != "pass" {
			t.Errorf("stale allowlist entry caused failure in %s: %+v", r.Name, r)
		}
	}
	if !strings.Contains(results[3].Summary, "warning: stale allowlist entries (path no longer tracked; delete the line):") || !strings.Contains(results[3].Summary, "gone.ts 950") {
		t.Fatalf("stale warning did not identify allowlist entry: %+v", results[3])
	}
}

// cannedExit is a hard command failure with a specific exit code, for
// the stubbed git grep in the restored Run-based tests above.
type cannedExit struct{ code int }

func (e cannedExit) Error() string { return "exit status" }
func (e cannedExit) ExitCode() int { return e.code }

func TestMalformedAllowlistAndMissingFilesFailClosed(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".githooks"), 0o700)
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt"} {
		data := []byte{}
		if name == "dir_entries_allowlist.txt" {
			data = []byte("malformed\n")
		}
		_ = os.WriteFile(filepath.Join(root, ".githooks", name), data, 0o600)
	}
	if _, err := Run(root, []string{"src/a.go"}, false, nil); err == nil {
		t.Fatal("malformed allowlist passed")
	}
	if err := os.WriteFile(filepath.Join(root, ".githooks", "dir_entries_allowlist.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(root, []string{"gone.go"}, false, nil); err == nil {
		t.Fatal("unreadable tracked file passed")
	}
}

func TestSizeGuardsDoNotExemptTestdataNodeModulesOrDist(t *testing.T) {
	// The shell mirrors enumerate every tracked .go/.ts/.tsx file; only the
	// directory guards skip testdata, node_modules and dist.
	for _, path := range []string{"src/testdata/big.go", "src/node_modules/big.ts", "src/dist/big.tsx"} {
		all := map[string]sourceFile{"src/main.go": {lines: 10}, path: {lines: 1101}}
		for _, limit := range []int{1000, 900} {
			r := sizeGuard("size", all, map[string]int{}, limit)
			if r.Status != "fail" || r.Files != 2 {
				t.Fatalf("%s at limit %d must fail with both files examined: %+v", path, limit, r)
			}
		}
	}
}

func TestStaleEntryWarningDoesNotHideAGenuineViolationExcerpt(t *testing.T) {
	all := map[string]sourceFile{"over.go": {lines: 950}}
	r := sizeGuard("file-size-900", all, map[string]int{"gone.go": 1000}, 900)
	if r.Status != "fail" || !strings.Contains(r.Excerpt, "over.go") {
		t.Fatalf("the first real violation must stay in the excerpt: %+v", r)
	}
}

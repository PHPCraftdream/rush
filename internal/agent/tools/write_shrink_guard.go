package tools

import "fmt"

// A worker replaced a 100-line registry document with one table row through
// write, silently (session wbgfix1, 2026-10-05): write has no "partial" mode, so
// a model that means a surgical change but sends only the changed part destroys
// the file. A write that would shrink an existing non-trivial file to a
// fraction of its size is refused unless the caller says it is deliberate.
const (
	// writeShrinkMinOldBytes: smaller files are cheap to recreate, never guarded.
	writeShrinkMinOldBytes = 2048
	// writeShrinkRatio: the new content must be at least 1/writeShrinkRatio of the
	// old size (a 75% reduction trips the guard).
	writeShrinkRatio = 4
)

func writeWouldShrinkFile(oldBytes, newBytes int) bool {
	return oldBytes >= writeShrinkMinOldBytes && newBytes*writeShrinkRatio < oldBytes
}

// writeShrinkRefusal is the refusal text; partialTools names the partial-edit
// tools of the toolset the caller is in (write's differ from fs_write's).
func writeShrinkRefusal(tool, partialTools, path string, oldBytes, newBytes int) string {
	return fmt.Sprintf(
		"Refusing to replace %s (%d bytes) with %d bytes of content: %s overwrites the WHOLE file, so sending only the changed part would delete the rest. "+
			"For a partial change use %s; if you really mean to replace the file with this much smaller content, repeat the call with allow_shrink=true. Nothing was written.",
		path, oldBytes, newBytes, tool, partialTools)
}

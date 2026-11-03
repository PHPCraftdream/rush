package cmd

import "testing"

func TestParseServePinSpecs(t *testing.T) {
	got, err := parseServePinSpecs([]string{"11111111=session-a", "22222222=session-b"})
	if err != nil || got["11111111"] != "session-a" || got["22222222"] != "session-b" {
		t.Fatalf("pins=%v error=%v", got, err)
	}
}

func TestParseServePinSpecsRejectsInvalidAndDuplicate(t *testing.T) {
	for _, specs := range [][]string{{"missing-separator"}, {"=session"}, {"token="}, {"token=one", "token=two"}} {
		if _, err := parseServePinSpecs(specs); err == nil {
			t.Fatalf("specs %v unexpectedly accepted", specs)
		}
	}
}

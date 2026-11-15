package tasktree

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestMutationPinsFingerprintCanonical(t *testing.T) {
	empty := ""
	command := Command{Op: OpEdit, Target: Selector{ID: "n1"}, ActiveForm: &empty}
	for _, tc := range []struct {
		revision Revision
		payload  string
	}{
		{0, `{"expected_revision":0,"command":{"op":"edit","target":{"id":"n1"},"active_form":""}}`},
		{7, `{"expected_revision":7,"command":{"op":"edit","target":{"id":"n1"},"active_form":""}}`},
	} {
		got, err := serviceFingerprint(tc.revision, command)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(tc.payload))
		want := hex.EncodeToString(digest[:])
		if got != want {
			t.Fatalf("revision=%d canonical fingerprint=%s want=%s", tc.revision, got, want)
		}
		without := command
		without.ActiveForm = nil
		nilHash, err := serviceFingerprint(tc.revision, without)
		if err != nil {
			t.Fatal(err)
		}
		if got == nilHash {
			t.Fatal("explicit empty active_form collides with absent pointer")
		}
		copyEmpty := ""
		command.ActiveForm = &copyEmpty
		copyHash, err := serviceFingerprint(tc.revision, command)
		if err != nil {
			t.Fatal(err)
		}
		if got != copyHash {
			t.Fatal("fingerprint depends on pointer address")
		}
	}
}

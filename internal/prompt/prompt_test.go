package prompt

import (
	"testing"
	"time"
)

func TestApprovalLike(t *testing.T) {
	cases := map[string]bool{
		"yes":                  true,
		"  Yes!! ":             true,
		"ok 👍":                 true,
		"Go ahead.":            true,
		"continue":             true,
		"yes but use postgres": false,
		"fix the failing test": false,
		"no":                   false,
		"":                     false,
		"continue with step 3": false,
	}
	for in, want := range cases {
		if got := ApprovalLike(in); got != want {
			t.Errorf("ApprovalLike(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	k := Key([]byte("salt"), "run:a", at)

	if Hash(k, "Continue ") != Hash(k, "continue") {
		t.Error("normalized prompts should hash the same")
	}
	if Hash(k, "yes") == Hash(k, "no") {
		t.Error("different prompts should differ")
	}
	if Hash(k, "yes") == Hash(Key([]byte("salt"), "run:a", at.Add(time.Millisecond)), "yes") {
		t.Error("the same prompt at another time should differ")
	}
	if Hash(k, "yes") == Hash(Key([]byte("salt"), "run:b", at), "yes") {
		t.Error("the same prompt in another session should differ")
	}
}

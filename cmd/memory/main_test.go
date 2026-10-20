package main

import (
	"strings"
	"testing"

	"agento/internal/memory"
)

// These pin the MCP writer's input mapping the way the gateway tests pin the
// HTTP contract: the two surfaces have drifted twice before (negative limits,
// topic strictness), so the mapping lives in pure funcs tested here.

func TestPrepareSave(t *testing.T) {
	it, truncated, errMsg := prepareSave("chat", saveMemoryInput{Topic: "Work Style", Text: "  prefers tables  "})
	if errMsg != "" {
		t.Fatalf("valid save: %q", errMsg)
	}
	if it.Profile != "chat" || it.Topic != "work-style" || it.Text != "prefers tables" ||
		it.Source != memory.SourceAgent || it.Status != memory.StatusPending {
		t.Fatalf("mapped item = %+v", it)
	}
	if truncated {
		t.Fatal("short text reported truncated")
	}

	// A NUL byte is stripped, not a truncation: the signal means length was
	// lost, matching CleanText vs Clipped.
	if _, truncated, errMsg := prepareSave("chat", saveMemoryInput{Topic: "fact", Text: "a\x00b"}); errMsg != "" || truncated {
		t.Fatalf("NUL-short save: truncated=%v err=%q", truncated, errMsg)
	}
	long, truncated, errMsg := prepareSave("chat", saveMemoryInput{Topic: "fact", Text: strings.Repeat("z", memory.MaxText+10)})
	if errMsg != "" || !truncated {
		t.Fatalf("long save: truncated=%v err=%q", truncated, errMsg)
	}
	if n := len([]rune(long.Text)); n != memory.MaxText {
		t.Fatalf("long save kept %d runes, want %d", n, memory.MaxText)
	}

	for _, tc := range []struct {
		name  string
		topic string
		text  string
	}{
		{"empty text", "fact", "   "},
		{"empty topic", "", "something"},
		{"punct topic", "---", "something"},
		{"slash topic", "a/b", "something"},
	} {
		if _, _, errMsg := prepareSave("chat", saveMemoryInput{Topic: tc.topic, Text: tc.text}); errMsg == "" {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
}

func TestPrepareList(t *testing.T) {
	f, errMsg := prepareList("chat", listMemoryInput{})
	if errMsg != "" || f.Limit != 50 || f.Profile != "chat" {
		t.Fatalf("defaults: %+v %q", f, errMsg)
	}
	f, errMsg = prepareList("chat", listMemoryInput{Topic: "Health", Status: "pending", Limit: 5})
	if errMsg != "" || f.Topic != "Health" || f.Status != memory.StatusPending || f.Limit != 5 {
		t.Fatalf("explicit: %+v %q", f, errMsg)
	}
	// Filters forgive: a junk topic passes through for the store to
	// normalise, unlike the write path.
	f, errMsg = prepareList("chat", listMemoryInput{Topic: "---"})
	if errMsg != "" || f.Topic != "---" {
		t.Fatalf("filter topic: %+v %q", f, errMsg)
	}
	for _, tc := range []struct {
		name string
		in   listMemoryInput
	}{
		{"bad status", listMemoryInput{Status: "Pending"}},
		{"negative limit", listMemoryInput{Limit: -1}},
	} {
		if _, errMsg := prepareList("chat", tc.in); errMsg == "" {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
}

func TestErrResultShape(t *testing.T) {
	_, out, _ := errResult("caller bug", false)
	for _, k := range []string{"ok", "error", "retryable", "persistent"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("error shape missing %q: %#v", k, out)
		}
	}
	if out["ok"] != false || out["retryable"] != false {
		t.Fatalf("validation error shape = %#v", out)
	}
}

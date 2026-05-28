package storage

import (
	"testing"
	"time"
)

func TestGroupByJobID(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)

	objects := []s3Object{
		{key: "abc/input.wav", modTime: old},
		{key: "abc/result.json", modTime: now},
		{key: "def/input.mp3", modTime: old},
		{key: "noslash", modTime: now},       // no "/" — must be skipped
		{key: "/leadingslash", modTime: now}, // empty first segment — must be skipped
	}

	got := groupByJobID(objects)

	if len(got) != 2 {
		t.Fatalf("expected 2 job entries, got %d", len(got))
	}

	abc := got["abc"]
	if len(abc.Keys) != 2 {
		t.Errorf("abc: expected 2 keys, got %d", len(abc.Keys))
	}
	if !abc.OldestModTime.Equal(old) {
		t.Errorf("abc: expected OldestModTime=%v, got %v", old, abc.OldestModTime)
	}

	def := got["def"]
	if len(def.Keys) != 1 {
		t.Errorf("def: expected 1 key, got %d", len(def.Keys))
	}
	if !def.OldestModTime.Equal(old) {
		t.Errorf("def: expected OldestModTime=%v, got %v", old, def.OldestModTime)
	}
}

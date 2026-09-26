package main

import "testing"

func TestBuildStitchedMarkdownWritesAllHeadingLevelsForFirstChunk(t *testing.T) {
	// Regression test: an earlier version updated prevHierarchy after
	// writing each individual depth level (not once per chunk), which
	// made depth 1+ compare against itself and get silently skipped --
	// so a chunk with parent_hierarchy ["Product X", "System Requirements"]
	// rendered only "# Product X" and dropped "## System Requirements"
	// entirely. Caught via a real end-to-end run against the test fixture,
	// not just this synthetic case.
	parts := []stitchPart{
		{ParentHierarchy: []string{"Product X", "System Requirements"}, Text: "body text one"},
	}
	got := buildStitchedMarkdown(parts)
	want := "# Product X\n\n## System Requirements\n\nbody text one"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildStitchedMarkdownDoesNotRepeatUnchangedHeadings(t *testing.T) {
	parts := []stitchPart{
		{ParentHierarchy: []string{"Product X", "System Requirements"}, Text: "first"},
		{ParentHierarchy: []string{"Product X", "System Requirements"}, Text: "second, same section"},
		{ParentHierarchy: []string{"Product X", "Deployment"}, Text: "third, new subsection"},
	}
	got := buildStitchedMarkdown(parts)
	want := "# Product X\n\n## System Requirements\n\nfirst\n\nsecond, same section\n\n## Deployment\n\nthird, new subsection"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildStitchedMarkdownHandlesEmptyHierarchy(t *testing.T) {
	parts := []stitchPart{
		{ParentHierarchy: nil, Text: "no heading context at all"},
	}
	got := buildStitchedMarkdown(parts)
	if got != "no heading context at all" {
		t.Fatalf("got %q", got)
	}
}

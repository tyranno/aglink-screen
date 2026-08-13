//go:build windows

package main

import (
	"strings"
	"testing"
)

// onelinePreview flattens an inlined snapshot preview to one bounded line — so a
// multi-line field value can't break the one-element-per-line snapshot format or
// blow the token budget.
func TestOnelinePreview(t *testing.T) {
	if got := onelinePreview("hello\r\nworld\ttab", 100); got != "hello world tab" {
		t.Errorf("newlines/tabs should collapse to single spaces, got %q", got)
	}
	if got := onelinePreview("a    b     c", 100); got != "a b c" {
		t.Errorf("runs of whitespace should collapse, got %q", got)
	}
	if got := onelinePreview("  trim me  ", 100); got != "trim me" {
		t.Errorf("should trim ends, got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := onelinePreview(long, 120)
	if r := []rune(got); len(r) != 121 || r[120] != '…' {
		t.Errorf("should cap to 120 runes + ellipsis, got len=%d", len([]rune(got)))
	}
	if onelinePreview("", 120) != "" {
		t.Error("empty stays empty")
	}
}

// clampTextValue is the belt-and-suspenders bound on get_text output on top of the
// server-side GetText truncation.
func TestClampTextValue(t *testing.T) {
	if got := clampTextValue("short"); got != "short" {
		t.Errorf("under-cap text unchanged, got %q", got)
	}
	over := strings.Repeat("y", maxTextChars+500)
	got := clampTextValue(over)
	if !strings.Contains(got, "truncated") {
		t.Errorf("over-cap text should be marked truncated")
	}
	if len([]rune(got)) <= maxTextChars {
		t.Errorf("truncated output should still carry maxTextChars of content plus a marker")
	}
}

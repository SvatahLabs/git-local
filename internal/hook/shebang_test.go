package hook

import "testing"

// Linux passes everything after a shebang's interpreter as one argument, so
// env runs a shell given options or assignments only through -S, and busybox
// takes exactly one applet name. macOS and the BSDs split the arguments.
func TestPosixShellFollowsKernelArgumentSplitting(t *testing.T) {
	for _, c := range []struct {
		shebang       string
		linux, splits bool
	}{
		{"#!/usr/bin/env sh", true, true},
		{"#!/usr/bin/env -S sh -e", true, true},
		{"#!/usr/bin/env -Ssh", true, true},
		{"#!/usr/bin/env -S -u NAME VAR=x sh", true, true},
		{"#!/bin/sh -e", true, true},
		{"#!/bin/busybox sh", true, true},
		{"#!/usr/bin/env -u NAME sh", false, true},
		{"#!/usr/bin/env VAR=x sh", false, true},
		{"#!/usr/bin/env -i sh", false, true},
		{"#!/usr/bin/env sh -e", false, true},
		{"#!/bin/busybox sh -e", false, true},
		{"#!/usr/bin/env python3", false, false},
		{"#!/usr/bin/env -S python3 sh", false, false},
	} {
		if got := posixShellOn(c.shebang, false); got != c.linux {
			t.Errorf("%q on Linux: %v, want %v", c.shebang, got, c.linux)
		}
		if got := posixShellOn(c.shebang, true); got != c.splits {
			t.Errorf("%q on a splitting kernel: %v, want %v", c.shebang, got, c.splits)
		}
	}
}

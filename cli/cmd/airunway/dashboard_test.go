package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseDashboardCompanion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launcher fixture is a POSIX script; Windows naming is covered separately")
	}
	version := "v9.8.7"
	built := filepath.Join(t.TempDir(), "airunway")
	if out, err := exec.Command("go", "build", "-ldflags", "-X github.com/ai-runway/airunway/cli/internal/cli.Version="+version, "-o", built, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	data, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	suffix := version + "-" + runtime.GOOS + "-" + runtime.GOARCH
	for _, tc := range []struct{ name, cli, web string }{
		{"release pair", "airunway-" + suffix, "airunway-web-" + suffix},
		{"renamed CLI", "airunway", "airunway-web-" + suffix},
		{"canonical pair", "airunway", "airunway-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, tc.cli)
			if err := os.WriteFile(binary, data, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.web), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"serve"}, {"login", "--server", "https://example.test", "--context", "fixture"}, {"logout"}} {
				cmd := exec.Command(binary, args...)
				cmd.Env = []string{"PATH=", "HOME=" + dir}
				out, err := cmd.CombinedOutput()
				expected := args
				if len(expected) == 0 {
					expected = []string{"serve"}
				}
				if err != nil || string(out) != strings.Join(expected, "\n")+"\n" {
					t.Fatalf("%v: %v %s", args, err, out)
				}
			}
		})
	}
}

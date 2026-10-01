package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the shipped entrypoint, not an in-process command handler.
func TestStandaloneBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "airunway")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	home := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"help", []string{"--help"}, "model create"},
		{"version", []string{"version", "--output=json"}, `"version"`},
		{"preview", []string{"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B", "--dry-run=client", "--output=json"}, `"ModelDeployment"`},
		{"completion", []string{"completion", "bash"}, "complete -W"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary, tc.args...)
			// With no PATH, neither Bun, Node, kubectl, nor an exec credential helper
			// can run. Help, version, completion, and client previews remain usable.
			cmd.Env = []string{"PATH=", "HOME=" + home, "AIRUNWAY_CONFIG=" + filepath.Join(home, "cli.json"), "KUBECONFIG=" + filepath.Join(home, "missing")}
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("%v\n%s", err, output)
			}
			if tc.name == "version" || tc.name == "preview" {
				if !json.Valid(output) {
					t.Fatalf("invalid JSON: %s", output)
				}
			}
		})
	}
	cmd := exec.Command(binary, "--unknown", "--output=json")
	cmd.Env = append(os.Environ(), "AIRUNWAY_CONFIG="+filepath.Join(home, "cli.json"))
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !json.Valid(output) {
		t.Fatalf("usage failure: %v %s", err, output)
	}
}

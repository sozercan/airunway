package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const companionTestWindows = "windows"

func TestDashboardCompanionKubeconfig(t *testing.T) {
	if runtime.GOOS == companionTestWindows {
		t.Skip("executable companion fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "airunway")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	explicit := filepath.Join(dir, "explicit kubeconfig")
	inherited := filepath.Join(dir, "inherited kubeconfig")
	for path, content := range map[string]string{
		explicit:  "synthetic-explicit-private-token\n",
		inherited: "synthetic-inherited-private-token\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Inspect the selected file without printing paths or credential contents.
	// PATH is empty, so no real dashboard, auth helper, or cluster CLI can run.
	script := `#!/bin/sh
[ "$#" -eq 5 ] || exit 40
[ "$1" = login ] && [ "$2" = --server ] && [ "$3" = https://example.test ] || exit 41
[ "$4" = --context ] && [ "$5" = fixture ] || exit 42
[ "$KUBECONFIG" = "$EXPECTED_KUBECONFIG" ] || exit 43
[ "$COMPANION_SENTINEL" = preserved ] || exit 44
IFS= read -r content < "$KUBECONFIG" || exit 45
[ "$content" = "$EXPECTED_CONTENT" ] || exit 46
printf 'companion-ok\n'
`
	if err := os.WriteFile(filepath.Join(dir, "airunway-web"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags []string
		path  string
		token string
	}{
		{"explicit overrides inherited", []string{"--kubeconfig", explicit}, explicit, "synthetic-explicit-private-token"},
		{"omitted preserves inherited", nil, inherited, "synthetic-inherited-private-token"},
		{"empty preserves inherited", []string{"--kubeconfig="}, inherited, "synthetic-inherited-private-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"login", "--server", "https://example.test", "--context", "fixture"}, tc.flags...)
			cmd := exec.Command(binary, args...)
			cmd.Env = []string{
				"PATH=", "HOME=" + dir, "KUBECONFIG=" + inherited,
				"EXPECTED_KUBECONFIG=" + tc.path, "EXPECTED_CONTENT=" + tc.token,
				"COMPANION_SENTINEL=preserved",
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("companion rejected its arguments, inherited environment, or selected kubeconfig: %v", err)
			}
			if string(output) != "companion-ok\n" {
				t.Fatal("unexpected companion output; credential contents must not be logged")
			}
		})
	}
}

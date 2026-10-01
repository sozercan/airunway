package cli

import (
	"strings"
	"testing"
)

func TestRuntimeImageRegistryPortRange(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, suffix := range []string{":v1", "@sha256:" + strings.Repeat("a", 64)} {
			for _, port := range []string{"0", "65536", "70000", "999999999999999999999999999", "", "bad", "-1"} {
				ref := "registry.example:" + port + "/runtime" + suffix
				if _, err := manifestImageReference(ref, "runtime image", pinned); err == nil {
					t.Errorf("accepted invalid port %q", port)
				}
			}
			for _, port := range []string{"1", "5000", "65535"} {
				ref := "registry.example:" + port + "/runtime" + suffix
				got, err := manifestImageReference(ref, "runtime image", pinned)
				if err != nil || got != ref {
					t.Errorf("valid reference changed or rejected: got=%q error=%v", got, err)
				}
			}
		}
	}
}

func TestRuntimeImageManifestValidation(t *testing.T) {
	for _, ref := range []string{"runtime", "runtime:v1", "registry.example:5000/runtime"} {
		if _, err := manifestImageReference(ref, "runtime image", false); err != nil {
			t.Errorf("valid unpinned reference rejected: %v", err)
		}
	}
	streams, _, _ := manifestTestIO("")
	_, err := manifestTestModelResult(Flags{"image": {"registry.example:70000/runtime:v1"}}, streams)
	manifestTestUsage(t, err, "registry port must be between 1 and 65535")
}

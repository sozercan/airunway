package cli

import (
	"reflect"
	"testing"
)

const (
	optionTestKAITO   = "kaito"
	optionTestKubeRay = "kuberay"
)

func TestRawEngineArgsProviderOptions(t *testing.T) {
	args := []string{"--max-num-seqs=8", "--enforce-eager"}
	for _, provider := range []string{optionTestKAITO, optionTestKubeRay, "vllm", "dynamo", "llmd", ""} {
		t.Run(provider, func(t *testing.T) {
			flags := Flags{"engine-arg": args}
			if provider != "" {
				flags["provider"] = []string{provider}
			}
			streams, _, _ := manifestTestIO("")
			got, err := manifestTestModelResult(flags, streams)
			if provider == optionTestKAITO || provider == optionTestKubeRay {
				manifestTestUsage(t, err, "--engine-arg is not supported by provider "+provider)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []any{args[0], args[1]}
			if !reflect.DeepEqual(get(got, "spec", "engine", "extraArgs"), want) {
				t.Fatalf("raw arguments changed: %v", get(got, "spec", "engine", "extraArgs"))
			}
		})
	}
}

func providerOptionModel(t *testing.T, provider, selection string) Object {
	t.Helper()
	existing := manifestTestModel(t, Flags{})
	if provider == "" {
		return existing
	}
	if selection == "status" {
		existing["status"] = Object{"provider": Object{"name": provider}}
	} else {
		object(existing["spec"])["provider"] = Object{"name": provider}
	}
	return existing
}

func TestRawEngineArgsProviderUpdateOptions(t *testing.T) {
	for _, provider := range []string{optionTestKAITO, optionTestKubeRay, "vllm", "dynamo", "llmd", ""} {
		for _, selection := range []string{"spec", "status"} {
			t.Run(provider+"/"+selection, func(t *testing.T) {
				existing := providerOptionModel(t, provider, selection)
				before := cloneObject(existing)
				streams, _, _ := manifestTestIO("")
				patch, err := updateResource("model", existing, Flags{"engine-arg": {"--max-num-seqs=8"}}, streams)
				if provider == optionTestKAITO || provider == optionTestKubeRay {
					manifestTestUsage(t, err, "--engine-arg is not supported by provider "+provider)
				} else {
					if err != nil {
						t.Fatal(err)
					}
					manifestTestEqual(t, get(patch, "spec", "engine", "extraArgs"), []any{"--max-num-seqs=8"})
				}
				manifestTestEqual(t, existing, before)
				// Other supported updates must remain possible.
				if _, err := updateResource("model", existing, Flags{"replicas": {"1"}}, streams); err != nil {
					t.Fatalf("update without raw arguments was rejected: %v", err)
				}
			})
		}
	}
}

func TestProviderDefaultsWithoutRawEngineArgs(t *testing.T) {
	for _, provider := range []string{optionTestKAITO, optionTestKubeRay, "vllm", "dynamo", "llmd"} {
		t.Run(provider, func(t *testing.T) {
			got := manifestTestModel(t, Flags{"provider": {provider}})
			if get(got, "spec", "engine", "extraArgs") != nil {
				t.Fatal("provider default unexpectedly contains raw arguments")
			}
		})
	}
}

func TestKAITOZeroReplicasProviderOptions(t *testing.T) {
	for _, zero := range []string{"0"} {
		streams, _, _ := manifestTestIO("")
		_, err := manifestTestModelResult(Flags{"provider": {"kaito"}, "replicas": {zero}}, streams)
		manifestTestUsage(t, err, "KAITO does not support zero replicas")
		for _, selection := range []string{"spec", "status"} {
			existing := providerOptionModel(t, "kaito", selection)
			_, err := updateResource("model", existing, Flags{"replicas": {zero}}, streams)
			manifestTestUsage(t, err, "KAITO does not support zero replicas")
		}
	}
	for _, provider := range []string{"vllm", "dynamo", "kuberay", "llmd", ""} {
		flags := Flags{"replicas": {"0"}}
		if provider != "" {
			flags["provider"] = []string{provider}
		}
		got := manifestTestModel(t, flags)
		manifestTestEqual(t, get(got, "spec", "scaling", "replicas"), 0)
	}
}

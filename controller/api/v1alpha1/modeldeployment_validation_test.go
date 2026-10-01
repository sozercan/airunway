/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateEngineArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      map[string]string
		extraArgs []string
		wantErr   bool
	}{
		{
			name: "no overlap is allowed",
			args: map[string]string{"gpu-memory-utilization": "0.9"},
			extraArgs: []string{
				"--enable-chunked-prefill",
				"--max-num-seqs=64",
			},
			wantErr: false,
		},
		{
			name:      "same key inline-value form in both is rejected",
			args:      map[string]string{"tensor-parallel-size": "4"},
			extraArgs: []string{"--tensor-parallel-size=2"},
			wantErr:   true,
		},
		{
			name:      "same key two-token form in both is rejected",
			args:      map[string]string{"tensor-parallel-size": "4"},
			extraArgs: []string{"--tensor-parallel-size", "2"},
			wantErr:   true,
		},
		{
			name:      "extraArgs-only flag is allowed (no conflict)",
			extraArgs: []string{"--tensor-parallel-size=2"},
			wantErr:   false,
		},
		{
			name:    "args-only flag is allowed (no conflict)",
			args:    map[string]string{"tensor-parallel-size": "4"},
			wantErr: false,
		},
		{
			name:      "bare positional token never collides",
			args:      map[string]string{"tensor-parallel-size": "4"},
			extraArgs: []string{"tensor-parallel-size"},
			wantErr:   false,
		},
		{
			name:    "empty inputs are allowed",
			wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &ModelDeploymentSpec{
				Engine: EngineSpec{
					Args:      tc.args,
					ExtraArgs: tc.extraArgs,
				},
			}
			err := spec.ValidateEngineArgs()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				for _, want := range []string{"spec.engine.args", "spec.engine.extraArgs"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q should mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestModelDeploymentZeroReplicasJSONRoundTrip(t *testing.T) {
	const input = `{"spec":{"model":{"id":"test/model"},"scaling":{"replicas":0}}}`
	var md ModelDeployment
	if err := json.Unmarshal([]byte(input), &md); err != nil {
		t.Fatal(err)
	}
	// Providers use typed updates for finalizers and spec changes.
	md.Finalizers = []string{"airunway.ai/test-provider"}
	md.Spec.Engine.Args = map[string]string{"max-model-len": "1024"}
	data, err := json.Marshal(&md)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Spec struct {
			Scaling struct {
				Replicas *int32 `json:"replicas"`
			} `json:"scaling"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Spec.Scaling.Replicas == nil || *wire.Spec.Scaling.Replicas != 0 {
		t.Fatalf("explicit zero was lost in typed update: %s", data)
	}
	var roundTrip ModelDeployment
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Spec.Scaling == nil || roundTrip.Spec.Scaling.Replicas != 0 {
		t.Fatalf("explicit zero was lost in JSON round trip: %+v", roundTrip.Spec.Scaling)
	}
}

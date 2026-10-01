package dynamo

import (
	"reflect"
	"strings"
	"testing"
)

func TestSummaryLiteralLaunches(t *testing.T) {
	for _, tc := range []struct {
		name          string
		command, args []any
		engine        string
		tp, pp        *int32
	}{
		{"direct argv", []any{"python3", "-m", "dynamo.vllm"}, []any{"-tp", "4", "-pp=2"}, "vllm", summaryDecimal("4", 1), summaryDecimal("2", 1)},
		{"module in args", []any{"/usr/bin/python3"}, []any{"-m", "dynamo.sglang", "--tp-size", "8"}, "sglang", summaryDecimal("8", 1), nil},
		{"literal shell", []any{"bash", "-c"}, []any{"exec python3 -m dynamo.vllm --tensor-parallel-size '4' \\\n --pipeline-parallel-size=\"2\" --kv-transfer-config '{\"kv_connector\":\"NixlConnector\"}'\n"}, "vllm", summaryDecimal("4", 1), summaryDecimal("2", 1)},
		{"trtllm config not interpreted", []any{"python3", "-m", "dynamo.trtllm"}, []any{"--extra-engine-args", "/config.yaml", "--tensor-parallel-size", "4"}, "trtllm", nil, nil},
		{"env expansion", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm --tensor-parallel-size $TP"}, "", nil, nil},
		{"command substitution", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm --tensor-parallel-size $(printf 4)"}, "", nil, nil},
		{"backtick substitution", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm --tensor-parallel-size `echo 4`"}, "", nil, nil},
		{"two commands", []any{"sh", "-c"}, []any{"echo ready && python3 -m dynamo.vllm --tensor-parallel-size 4"}, "", nil, nil},
		{"two lines", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm\npython3 -m dynamo.vllm --tensor-parallel-size 4"}, "", nil, nil},
		{"shell positional arguments", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm", "--tensor-parallel-size", "4"}, "", nil, nil},
		{"shell unmatched quote", []any{"sh", "-c"}, []any{"python3 -m dynamo.vllm --tensor-parallel-size '4"}, "", nil, nil},
		{"arbitrary script", []any{"python3", "/start.py"}, []any{"--tensor-parallel-size", "4"}, "", nil, nil},
		{"arbitrary module", []any{"python3", "-m", "custom.vllm"}, []any{"--tensor-parallel-size", "4"}, "", nil, nil},
		{"no explicit entrypoint", nil, []any{"--tensor-parallel-size", "4"}, "", nil, nil},
		{"malformed argv", []any{"python3", int64(2)}, nil, "", nil, nil},
		{"duplicate aliases", []any{"python3", "-m", "dynamo.sglang"}, []any{"--tp", "4", "--tp-size=8"}, "sglang", nil, nil},
		{"same repeated flag", []any{"python3", "-m", "dynamo.vllm"}, []any{"-tp", "4", "-tp", "4"}, "vllm", nil, nil},
		{"missing value", []any{"python3", "-m", "dynamo.vllm"}, []any{"--tensor-parallel-size", "--model", "Qwen/model"}, "vllm", nil, nil},
		{"fractional value", []any{"python3", "-m", "dynamo.vllm"}, []any{"--tensor-parallel-size", "4.5"}, "vllm", nil, nil},
		{"overflow", []any{"python3", "-m", "dynamo.vllm"}, []any{"--tensor-parallel-size=2147483648"}, "vllm", nil, nil},
		{"no zero default", []any{"python3", "-m", "dynamo.vllm"}, []any{"--tensor-parallel-size=0"}, "vllm", nil, nil},
		{"unknown config", []any{"python3", "-m", "dynamo.vllm"}, []any{"--config", "/config.yaml", "--tensor-parallel-size=4"}, "vllm", nil, nil},
		{"terminated args", []any{"python3", "-m", "dynamo.vllm"}, []any{"--", "--tensor-parallel-size", "4"}, "vllm", nil, nil},
		{"quoted text is not a flag", []any{"python3", "-m", "dynamo.vllm"}, []any{"--chat-template", "--tensor-parallel-size 4"}, "vllm", nil, nil},
		{"oversized script", []any{"sh", "-c"}, []any{strings.Repeat("x", 32769)}, "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main := map[string]any{"command": tc.command, "args": tc.args}
			engine, args := summaryLaunch(main)
			tp, pp := summaryParallelism(engine, args)
			if engine != tc.engine || !reflect.DeepEqual(tp, tc.tp) || !reflect.DeepEqual(pp, tc.pp) {
				t.Fatalf("got engine=%q TP=%v PP=%v; want engine=%q TP=%v PP=%v", engine, tp, pp, tc.engine, tc.tp, tc.pp)
			}
		})
	}
}

func TestSummaryConflictingLaunchMetadata(t *testing.T) {
	dgd := summaryFixture(true, "prefill", "vllm", "--disaggregation-mode", "decode")
	dgd.Object["spec"].(map[string]any)["backendFramework"] = "sglang"
	plan := intentPlan(nil, dgd)
	if plan.Engine != "" || plan.ServingMode != "" || plan.Workers[0].Role != "" {
		t.Fatalf("conflicting metadata must stay unknown: %+v", plan)
	}
	c := summaryFixtureComponent(dgd)
	pod := c["podTemplate"].(map[string]any)["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	pod["containers"] = append(containers, containers[1])
	if main := summaryMainContainer(c); main != nil {
		t.Fatal("two main containers are ambiguous")
	}
}

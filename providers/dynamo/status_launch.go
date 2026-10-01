package dynamo

import (
	"path"
	"strconv"
	"strings"
)

// summaryLaunch accepts a direct Python module launch or a single literal shell
// command. It never evaluates shell code, follows scripts, reads config files,
// or guesses a backend from an image or component name.
func summaryLaunch(container map[string]any) (string, []string) {
	command, ok := summaryArgv(container["command"])
	if !ok || len(command) == 0 {
		return "", nil
	}
	args, ok := summaryArgv(container["args"])
	if !ok {
		return "", nil
	}
	argv := append(command, args...)
	if shell := path.Base(argv[0]); shell == "sh" || shell == "bash" {
		if len(argv) != 3 || argv[1] != "-c" {
			return "", nil
		}
		argv, ok = summaryShellWords(argv[2])
		if !ok || len(argv) == 0 {
			return "", nil
		}
		if argv[0] == "exec" {
			argv = argv[1:]
		}
	}
	if len(argv) < 3 || (path.Base(argv[0]) != "python" && path.Base(argv[0]) != "python3") || argv[1] != "-m" {
		return "", nil
	}
	engine := strings.TrimPrefix(argv[2], "dynamo.")
	if argv[2] != "dynamo."+engine || !summaryEngine(engine) {
		return "", nil
	}
	return engine, argv[3:]
}

func summaryEngine(engine string) bool {
	return engine == "vllm" || engine == "sglang" || engine == "trtllm"
}

func summaryArgv(raw any) ([]string, bool) {
	if raw == nil {
		return nil, true
	}
	values, ok := raw.([]any)
	if !ok || len(values) > 256 {
		return nil, false
	}
	args := make([]string, 0, len(values))
	size := 0
	for _, raw := range values {
		arg, ok := raw.(string)
		size += len(arg)
		if !ok || size > 32768 {
			return nil, false
		}
		args = append(args, arg)
	}
	return args, true
}

// This intentionally is not a general shell parser. Reject expansion, control
// operators, comments, globs and newlines except escaped line continuations.
func summaryShellWords(script string) ([]string, bool) {
	script = strings.TrimSpace(script)
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(script); i++ {
		c := script[i]
		if c == 0 || c == '$' || c == '`' {
			return nil, false
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				// Only unambiguous quoted escapes are supported.
				if i+1 >= len(script) || !strings.ContainsRune("\\\"\n", rune(script[i+1])) {
					return nil, false
				}
				i++
				if script[i] != '\n' {
					word.WriteByte(script[i])
				}
			} else {
				word.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote, started = c, true
		case '\\':
			if i+1 >= len(script) || script[i+1] != '\n' {
				return nil, false
			}
			i++
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case '\n', '\r', ';', '|', '&', '<', '>', '(', ')', '{', '}', '*', '?', '[', ']', '#', '~':
			return nil, false
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, len(words) <= 256
}

// Repeated aliases, missing values and option terminators are not evidence for
// a particular setting. Do not implement an assumed last-value-wins policy.
func summaryOption(args []string, names ...string) (value string, present bool, valid bool) {
	valid = true
	for i, arg := range args {
		if arg == "--" {
			break
		}
		key, inline, hasValue := strings.Cut(arg, "=")
		for _, name := range names {
			if key != name {
				continue
			}
			if present {
				valid = false
			}
			present = true
			if hasValue {
				value = inline
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				value = args[i+1]
			} else {
				valid = false
			}
			if value == "" {
				valid = false
			}
		}
	}
	return
}

func summaryParallelism(engine string, args []string) (*int32, *int32) {
	// TRT-LLM uses structured extra-engine arguments. Config-file settings and
	// arbitrary wrappers cannot be resolved safely from this snapshot.
	if engine != "vllm" && engine != "sglang" {
		return nil, nil
	}
	if _, present, _ := summaryOption(args, "--config", "--config-file", "--yaml-config"); present {
		return nil, nil
	}
	tpNames, ppNames := []string{"--tensor-parallel-size", "-tp"}, []string{"--pipeline-parallel-size", "-pp"}
	if engine == "sglang" {
		tpNames, ppNames = []string{"--tensor-parallel-size", "--tp-size", "--tp"}, []string{"--pipeline-parallel-size", "--pp-size", "--pp"}
	}
	return summaryIntegerOption(args, tpNames...), summaryIntegerOption(args, ppNames...)
}

func summaryIntegerOption(args []string, names ...string) *int32 {
	value, present, valid := summaryOption(args, names...)
	if !present || !valid {
		return nil
	}
	return summaryDecimal(value, 1)
}

func summaryDecimal(value string, minimum int32) *int32 {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < int64(minimum) {
		return nil
	}
	result := int32(n)
	return &result
}

// Dynamo 1.1.1 and 1.5.0 default vLLM/TRT-LLM to agg and SGLang to
// null (non-disaggregated). Only apply that documented default to their
// pinned runtime images and an unambiguous launch without mode overrides.
func summaryAggregatedLaunch(object, component, container map[string]any, engine string, args []string) bool {
	if !summaryEngine(engine) {
		return false
	}
	repository := engine + "-runtime"
	if engine == "trtllm" {
		repository = "tensorrtllm-runtime"
	}
	image, _ := container["image"].(string)
	prefix := "nvcr.io/nvidia/ai-dynamo/" + repository + ":"
	if image != prefix+"1.1.1" && image != prefix+"1.5.0" {
		return false
	}
	if _, present, _ := summaryOption(args, "--config", "--config-file", "--yaml-config", "--disagg-config", "--disagg-config-key",
		"--is-prefill-worker", "--is-decode-worker", "--multimodal-worker", "--multimodal-prefill-worker", "--multimodal-decode-worker", "--multimodal-encode-worker"); present {
		return false
	}
	spec, _ := object["spec"].(map[string]any)
	for _, scope := range []map[string]any{spec, component, container} {
		if imports, ok := scope["envFrom"].([]any); ok && len(imports) > 0 {
			return false // Imported values are not visible in the selected plan.
		}
		if secret, _ := scope["envFromSecret"].(string); secret != "" {
			return false
		}
		for _, field := range []string{"env", "envs"} {
			entries, _ := scope[field].([]any)
			for _, raw := range entries {
				entry, _ := raw.(map[string]any)
				name, _ := entry["name"].(string)
				if strings.Contains(name, "DISAGG") || strings.Contains(name, "IS_PREFILL_WORKER") || strings.Contains(name, "IS_DECODE_WORKER") || strings.Contains(name, "MULTIMODAL") {
					return false
				}
			}
		}
	}
	mode, present, valid := summaryOption(args, "--disaggregation-mode")
	if !present {
		return true
	}
	return valid && (mode == "agg" && engine != "sglang" || mode == "null" && engine == "sglang")
}

package cli

import (
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"
)

var booleanFlags = strings.Fields("help version all-namespaces follow check wait trust-remote-code gateway timestamps")
var valueFlags = strings.Fields("kubeconfig context namespace output timeout id gpus cpu memory provider engine image model-path served-name context-length replicas credential revision file storage-size storage-class artifact-image service-account framework model-ref prompt prompt-file model-url model-api model-id model-credential model-gateway gateway-listener mode task task-file config-file preset dry-run for tail pod container port message message-file type from-file server temperature max-tokens")
var globalOptions = strings.Fields("kubeconfig context namespace output timeout help version")
var createOptions = strings.Fields("id gpus cpu memory provider engine image model-path served-name context-length replicas credential revision file storage-size storage-class artifact-image service-account engine-arg trust-remote-code gateway framework model-ref prompt prompt-file model-url model-api model-id model-credential model-gateway gateway-listener mode task task-file config-file preset dry-run wait")
var namePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
var durationPattern = regexp.MustCompile(`^\d+(ms|s|m|h)$`)

func parseArgs(argv []string) ([]string, Flags, error) {
	set := pflag.NewFlagSet("airunway", pflag.ContinueOnError)
	set.SetOutput(io.Discard)
	short := map[string]string{"output": "o", "namespace": "n", "help": "h", "follow": "f", "version": "v", "server": "s", "context": "c"}
	for _, key := range booleanFlags {
		set.BoolP(key, short[key], false, "")
	}
	for _, key := range valueFlags {
		set.StringP(key, short[key], "", "")
	}
	set.StringArray("engine-arg", nil, "")
	if err := set.Parse(argv); err != nil {
		return nil, nil, usage("Invalid command-line options. Run airunway --help.")
	}
	flags := Flags{}
	set.Visit(func(f *pflag.Flag) {
		if f.Name == "engine-arg" {
			flags[f.Name], _ = set.GetStringArray(f.Name)
		} else {
			flags[f.Name] = []string{f.Value.String()}
		}
	})
	return set.Args(), flags, nil
}

func requestedOutput(argv []string) string {
	result := ""
	for i, arg := range argv {
		if arg == "--" {
			break
		}
		if (arg == "--output" || arg == "-o") && i+1 < len(argv) {
			result = argv[i+1]
		}
		if strings.HasPrefix(arg, "--output=") {
			result = strings.TrimPrefix(arg, "--output=")
		}
		if strings.HasPrefix(arg, "-o=") {
			result = strings.TrimPrefix(arg, "-o=")
		} else if strings.HasPrefix(arg, "-o") && len(arg) > 2 {
			result = arg[2:]
		}
	}
	return result
}
func assertFlags(flags Flags, allowed []string) error {
	set := map[string]bool{}
	for _, key := range append(append([]string{}, globalOptions...), allowed...) {
		set[key] = true
	}
	for key := range flags {
		if !set[key] {
			return usage("Unsupported option --" + key + " for this command.")
		}
	}
	return nil
}
func assertPositionals(words []string, count int) error {
	if len(words) != count {
		return usage("Unexpected or missing command arguments. Run with --help.")
	}
	return nil
}
func validateName(value, label string) error {
	if !namePattern.MatchString(value) || len(value) > 63 {
		return usage("Provide a " + label + " of at most 63 lowercase letters, numbers, or hyphens, starting with a letter.")
	}
	return nil
}
func validateNamespace(value string) error {
	if len(validation.IsDNS1123Label(value)) != 0 {
		return usage("Provide a namespace of at most 63 lowercase letters, numbers, or hyphens, starting and ending with a letter or number.")
	}
	return nil
}
func parseDuration(value string) (time.Duration, error) {
	if value == "" {
		return 10 * time.Minute, nil
	}
	if !durationPattern.MatchString(value) {
		return 0, usage("Use a positive timeout such as 30s, 10m, or 1h.")
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < time.Millisecond || d > 24*time.Hour {
		return 0, usage("Timeout must be between 1ms and 24h.")
	}
	return d, nil
}
func dryRun(flags Flags) (string, error) {
	value := flags.Text("dry-run")
	if flags.Has("dry-run") && value != "client" && value != "server" {
		return "", usage("--dry-run must be client or server.")
	}
	return value, nil
}
func mergeAgentDefaults(flags, defaults Flags) Flags {
	merged := defaults.Copy()
	bindings := strings.Fields("model-ref model-url model-gateway model-id model-api model-credential gateway-listener")
	for _, key := range bindings {
		if flags.Has(key) {
			for _, binding := range bindings {
				delete(merged, binding)
			}
			break
		}
	}
	for key, value := range flags {
		merged[key] = append([]string(nil), value...)
	}
	return merged
}

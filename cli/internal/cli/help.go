package cli

import "strings"

const help = `AI Runway

Usage: airunway <resource> <action> [name] [options]

Models:
  model create NAME --id hf://ORG/MODEL [--gpus 1] [--provider NAME] [--engine NAME]
  model create NAME --image IMAGE --model-path /models/model [--provider vllm]
  model list [--all-namespaces]
  model get NAME
  model update NAME [--replicas N] [--context-length N] [--gpus N] [--memory SIZE]
  model delete NAME [--wait=false]
  model wait NAME --for ready [--timeout 20m]
  model endpoint NAME [--check [--credential NAME]]
  model connect NAME [--port 8000]
  model chat NAME [--message TEXT | --message-file FILE|-]
                  [--system TEXT | --system-file FILE|-] [--temperature N] [--max-tokens N]
  model logs NAME [--follow] [--tail N] [--pod NAME] [--container NAME]
  model events NAME

Agents:
  agent create NAME --framework FRAMEWORK --model-ref MODEL --prompt TEXT
  agent create NAME --framework FRAMEWORK --model-url URL --model-api TYPE --model-id ID
  agent create NAME --framework FRAMEWORK --model-gateway NAME --model-id ID
  agent create NAME --preset FRAMEWORK/PRESET --model-ref MODEL
  agent create NAME --framework FRAMEWORK --model-ref MODEL --mode once --task-file FILE
  agent run NAME --framework FRAMEWORK --model-ref MODEL --task-file FILE
  agent list | get NAME | delete NAME | events NAME
  agent update NAME [--prompt-file FILE|-] [--model-ref MODEL]
  agent wait NAME --for ready|completed [--timeout 10m]
  agent endpoint NAME [--check]
  agent connect NAME [--port 8080]
  agent chat NAME [--message TEXT | --message-file FILE|-]
  agent logs NAME [--follow] [--tail N]

Model creation options:
  --id REF                 hf://, s3://, gs://, https://, oci://, pvc://; bare ORG/MODEL means HF
  --revision REV           Source revision; --file selects a relative artifact file
  --credential NAME       Source credential in the selected namespace
  --storage-size SIZE     Staged artifact volume capacity (default 100Gi)
  --storage-class NAME    Volume storage class for staged artifacts
  --storage-access-mode MODE  ReadWriteOnce|ReadWriteMany for staged artifact volumes
                              Default ReadWriteOnce for 0 or 1 replicas, ReadWriteMany for 2+
  --artifact-image IMAGE  Override the artifact downloader image
  --service-account NAME Preconfigured artifact download workload identity
  --image IMAGE           Inference runtime image, not model artifacts
  --served-name NAME      Name inference clients send
  --gpus N                Requested GPUs (default 1, use 0 for CPU)
  --cpu QUANTITY          Requested CPU; --memory SIZE requests memory
  --replicas N            Running copies (default 1)
  --context-length N      Maximum model context length
  --engine-arg=ARG        Repeatable raw engine flag; --trust-remote-code is opt-in
  --gateway=false         Disable gateway integration; for access choose the internal Service
  --gateway-listener NAME Select an access listener when several match
  --server URL            Explicitly trust an external published endpoint for access
  --credential NAME       For model chat, a separate API_KEY ingress credential

Agent creation options:
  --prompt TEXT | --prompt-file FILE|-   Persistent system instructions
  --task TEXT | --task-file FILE|-       Task input for --mode once
  --config-file FILE      Framework-specific JSON configuration
  --model-credential NAME/KEY            External API credential reference
  --model-api openai|anthropic|azure-openai|custom
  --gateway-listener NAME               Select an existing gateway listener
  --image IMAGE           Override the framework catalog runtime image
  --cpu QUANTITY --memory SIZE           Container-backed agent resource requests

Chat options:
  --system TEXT | --system-file FILE|-   Model chat system instructions
  --temperature N          Sampling temperature, between 0 and 2
  --max-tokens N           Positive maximum completion token count

Environment and discovery:
  context list | current | use NAME
  config get | set KEY VALUE | unset KEY
  doctor
  provider list | get NAME
  framework list | get NAME
  catalog model search QUERY | get hf://ORG/MODEL
  catalog agent list | get FRAMEWORK/PRESET

Credentials (never print secret values):
  credential create NAME --type huggingface|api-key|artifact --from-file FILE|-
  credential list | get NAME
  credential update NAME --from-file FILE|-
  credential delete NAME

Declarative files:
  apply --file FILE|DIRECTORY [--dry-run client|server]

Global options:
  --kubeconfig FILE --context NAME --namespace NAME, -n NAME
  --output text|json|yaml, -o FORMAT
                          Text summaries by default; JSON/YAML for structured details
  --timeout DURATION      Waits and finite access default to 10m (maximum 24h)
                          Foreground sessions have no default timeout; supports ms, s, m, h
  --wait=false            Return after submission; timeouts do not delete resources
  --dry-run client|server Preview create/apply without persisting anything
  --help, -h              Show help without connecting to a cluster

agent run executes once, waits for completion, and prints the task result.
It retains the agent for inspection; timeout does not cancel or resubmit work.
Without --image, container frameworks use their unambiguous registered catalog
image. Use an explicit --image or --preset if the catalog has multiple images.

Settings: namespace, agent.framework, agent.model-ref. Agent defaults are per
context and namespace. Explicit model bindings replace the entire saved binding.

Other commands:
  serve                   Start airunway-web (also the no-argument default)
  login --server URL      Existing dashboard authentication
  logout                  Clear existing dashboard authentication
  version                 Show version information
  completion bash|zsh|fish Generate shell completion

Exit codes: 0 success, 1 operation failed, 2 invalid input, 3 auth/connectivity,
4 timeout, 5 conflict, 130 interrupted. Progress goes to stderr. Local connections
stay in the foreground and preserve upstream authentication. Chat may invoke tools.
`

func completion(shell string) (string, error) {
	commands := "model agent context config doctor credential provider framework catalog apply completion version serve login logout"
	flags := append(append(append([]string{}, booleanFlags...), valueFlags...), "engine-arg")
	for i := range flags {
		flags[i] = "--" + flags[i]
	}
	words := commands + " " + strings.Join(flags, " ")
	switch shell {
	case "bash":
		return "complete -W '" + words + "' airunway\n", nil
	case "zsh":
		return "#compdef airunway\n_arguments '*: :(" + words + ")'\n", nil
	case "fish":
		return "complete -c airunway -f -a '" + words + "'\n", nil
	default:
		return "", usage("Supported shells: bash, zsh, fish.")
	}
}

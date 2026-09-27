# Command-line interface

The `airunway` binary manages models and agents directly through the selected
Kubernetes API. It does not need the dashboard to be running. Running `airunway`
without arguments or running `airunway serve` still starts the dashboard.

Install the controller, matching CRDs, and the model/agent providers before using
creation commands. The CLI never creates clusters, namespaces, providers, cloud
identities, or management credentials automatically.

## Names and identifiers

Commands follow `airunway RESOURCE ACTION NAME`. The positional name identifies
one deployed instance. Model `--id` identifies the model artifacts. Agent
`--model-ref` identifies an existing model deployment in the selected namespace.

```bash
airunway model create demo --id hf://Qwen/Qwen3-8B --gpus 1
airunway agent create assistant --framework langgraph --model-ref demo \
  --prompt "You are a concise, helpful assistant."
```

Bare `Qwen/Qwen3-8B` is shorthand for `hf://Qwen/Qwen3-8B`. It always means Hugging
Face, regardless of local configuration. A name is not the source model ID or a
server-generated resource UID. `create` fails on an existing name; it never
silently updates or replaces it.

## Environment and defaults

```bash
airunway context list
airunway context current
airunway context use dev
airunway config set namespace team-a --context dev
airunway config get --context dev --namespace team-a
airunway doctor --context dev --namespace team-a

airunway model list --kubeconfig ./dev.kubeconfig --context dev --namespace team-a
```

Explicit flags override AI Runway's saved settings, which override kubeconfig
settings. `context use` changes only AI Runway's default, not kubectl's current
context. Settings live in `$XDG_CONFIG_HOME/airunway/cli.json`, or
`~/.config/airunway/cli.json`. `AIRUNWAY_CONFIG` selects another file. The CLI writes
configuration atomically with owner-only permissions.

Agent defaults are scoped to both context and namespace:

```bash
airunway config set agent.framework langgraph --context dev --namespace team-a
airunway config set agent.model-ref demo --context dev --namespace team-a
airunway agent create assistant --prompt "Be helpful."
airunway config unset agent.model-ref
```

An explicitly selected model binding replaces the entire saved binding. Defaults
are for new requests, never for rewriting existing deployments.

## Models

```bash
airunway model create demo --id hf://Qwen/Qwen3-8B --gpus 1 \
  --provider vllm --engine vllm --memory 32Gi --context-length 8192

airunway model create private-demo --id hf://example-org/private-model \
  --credential hf-access --gpus 1

airunway model list
airunway model list --all-namespaces
airunway model get demo --output json
airunway model logs demo --follow
airunway model events demo
airunway model update demo --replicas 2
airunway model update demo --context-length 16384
airunway model delete demo
```

The default GPU request is one. Use `--gpus 0` for a compatible CPU runtime.
Resource values in these examples are not a promise that a model fits every GPU.
Provider and engine selection remain controller-owned when omitted. `--served-name`
sets an inference-facing model name; endpoint discovery reports the resolved name
and routing headers rather than assuming the deployment name is callable.

Use `--engine-arg=--flag=value` repeatedly for raw engine flags. Remote model code
is not trusted unless `--trust-remote-code` is explicitly supplied. Use
`--gateway=false` to disable gateway integration, not to create a public service.

### Source references and artifacts

Remote artifacts are staged by the model downloader into a writable model-cache
volume. This path requires the matching controller, downloader image, and Direct
vLLM provider. Source adapters do not imply that every inference engine supports
every artifact format.

For a development checkout, build and publish `images/model-downloader` to your
own test registry. Set `ARTIFACT_DOWNLOADER_IMAGE` to that exact image and pass it
with `--artifact-image` as below. Do not assume an existing `latest` image includes
this checkout's artifact loader.

```bash
# A pinned Hugging Face snapshot
airunway model create pinned-demo --id hf://Qwen/Qwen3-8B \
  --revision MODEL_COMMIT --provider vllm --gpus 1 \
  --artifact-image "$ARTIFACT_DOWNLOADER_IMAGE"

# Download a remote prefix
airunway model create s3-demo --id s3://model-bucket/qwen3-8b/ \
  --artifact-image "$ARTIFACT_DOWNLOADER_IMAGE" \
  --credential object-storage --storage-size 100Gi --gpus 1

airunway model create gcs-demo --id gs://model-bucket/qwen3-8b/ \
  --service-account model-reader --gpus 1 \
  --artifact-image "$ARTIFACT_DOWNLOADER_IMAGE"

# HTTPS identifies artifact bytes, not an inference API
airunway model create file-demo \
  --id https://account.blob.core.windows.net/models/model.gguf \
  --file model.gguf --credential blob-access --gpus 1 \
  --artifact-image "$ARTIFACT_DOWNLOADER_IMAGE"

# OCI model artifacts, not executable container images
airunway model create registry-demo \
  --id oci://registry.example.com/models/qwen3-8b:v1 --gpus 1 \
  --artifact-image "$ARTIFACT_DOWNLOADER_IMAGE"

# Use an existing volume without downloading or owning it
airunway model create volume-demo --id pvc://model-store/qwen3-8b/ --gpus 1
```

`--storage-class` selects the staged artifact volume's class.
`--artifact-image` overrides the downloader image, not the inference image.
`--service-account` selects an existing download-job workload identity. Configure
cloud identity and permissions separately; the CLI cannot infer or create them.

Source URLs cannot contain credentials or signed query strings. `--revision` is
for Hugging Face; OCI identity is its tag or digest. `--file` must be a safe
relative path. Absolute paths and parent traversal are rejected. Source identity
is immutable after creation.

Runtime images and bundled model paths are separate:

```bash
airunway model create custom-runtime --id hf://Qwen/Qwen3-8B \
  --provider vllm --image registry.example.com/vllm-runtime:v1 --gpus 1

airunway model create bundled-demo --provider vllm \
  --image registry.example.com/qwen-server:v1 \
  --model-path /models/qwen3-8b --gpus 1
```

`--model-path` is inside the runtime image, not on the developer's laptop. Local
`file://` paths are rejected; upload artifacts to supported storage first.

## Agents and bindings

The default mode creates a long-running agent. Its prompt is persistent system
instructions, not a request to execute a task immediately.

```bash
airunway agent create assistant --framework langgraph --model-ref demo \
  --prompt-file ./instructions.md

cat ./instructions.md | airunway agent create assistant \
  --framework langgraph --model-ref demo --prompt-file -
```

Select exactly one model-binding form:

```bash
# An existing model deployment in this namespace
airunway agent create local-assistant --framework langgraph \
  --model-ref demo --prompt "Be helpful."

# An existing inference API; no model deployment is created
airunway agent create remote-assistant --framework langgraph \
  --model-url https://models.example.com/v1 --model-api openai \
  --model-id qwen-chat --model-credential inference-access/API_KEY \
  --prompt-file ./instructions.md

# Azure OpenAI: model ID is the configured deployment name
airunway agent create azure-assistant --framework langgraph \
  --model-url https://my-resource.openai.azure.com --model-api azure-openai \
  --model-id my-chat-deployment --model-credential azure-access/API_KEY \
  --prompt "Be helpful."

# Existing gateway and served-model name
airunway agent create gateway-assistant --framework langgraph \
  --model-gateway inference --gateway-listener https \
  --model-id team-chat --prompt "Be helpful."
```

Supported API types are `openai`, `anthropic`, `azure-openai`, and `custom`, subject
to framework compatibility. Credential references are namespace-local. The
requesting identity must be allowed to read the referenced credential.

```bash
airunway agent list
airunway agent get assistant
airunway agent logs assistant --follow
airunway agent events assistant
airunway agent update assistant --prompt-file ./revised-instructions.md
airunway agent update assistant --model-ref demo-v2
airunway agent delete assistant
```

Deleting an agent does not delete its shared model or user-managed credentials.
Changing the framework requires a new agent. Container image/resource overrides
are rejected for frameworks that do not implement them.

### Framework configuration and presets

```bash
airunway framework list
airunway framework get langgraph
airunway catalog agent list
airunway catalog agent get langgraph/basic-assistant

airunway agent create assistant --preset langgraph/basic-assistant --model-ref demo

airunway agent create research-assistant --framework langgraph --model-ref demo \
  --config-file ./langgraph.json --prompt-file ./instructions.md
```

Use preset identifiers returned by the installed catalog. A preset selects
framework configuration; it cannot silently create a model or install a provider.
`--config-file` contains framework-specific JSON, not a whole deployment. Conflicts
between explicit flags and file configuration are rejected.

### One-shot tasks

```bash
airunway agent create report --framework crewai --model-ref demo --mode once \
  --prompt "Summarize the supplied information accurately." \
  --task-file ./report-task.txt --wait=false

airunway agent wait report --for completed --timeout 10m
airunway agent logs report
airunway agent delete report
```

One-shot mode requires a compatible container framework. It has no endpoint or
interactive chat. Creation submits work. Jobs may retry, so execution is not
exactly-once. The CLI never resubmits automatically after timeout. Use a new name
for another task; ordinary updates must not replay a completed job.

## Readiness and access

```bash
airunway model wait demo --for ready --timeout 20m
airunway model endpoint demo
airunway model endpoint demo --check --output json
airunway model connect demo --port 8000
airunway model chat demo --message "Reply with OK."

airunway agent wait assistant --for ready
airunway agent endpoint assistant
airunway agent connect assistant --port 8080
airunway agent chat assistant
airunway agent chat assistant --message-file ./question.txt
```

Deployment readiness, endpoint reachability, and successful inference are separate
claims. Endpoint checks do not submit chat requests or execute agent tools.
Provider status must publish a usable access contract. Providers without one fail
with an explanation rather than a fabricated URL.

`connect` binds loopback and stays in the foreground. Ctrl+C closes the connection,
not the deployment. Authentication remains required by the upstream service.
`chat` can resolve authorized agent ingress credentials internally; it never uses
the model credential as an agent-call token. Chat requests may execute configured
tools, so use trusted frameworks and deliberate prompts.

## Credentials

```bash
airunway credential create hf-access --type huggingface --from-file ./hf-token.txt
airunway credential create inference-access --type api-key --from-file - < ./key.txt
airunway credential create object-storage --type artifact --from-file ./storage.json

airunway credential list
airunway credential get hf-access
airunway credential update hf-access --from-file ./replacement.txt
airunway credential delete hf-access
```

Commands affect only the selected namespace. Hugging Face uses `HF_TOKEN`, API
keys use `API_KEY`, and artifact loaders use source-specific JSON in `credentials`.
`get` and `list` return metadata, never secret bytes. Update/delete operate only on
CLI-managed credentials. Referenced credentials cannot be deleted through this
command. No tokens belong in command arguments, model URLs, config files checked
into Git, or machine-readable output.

## Files, previews, and automation

```bash
airunway model create demo --id hf://Qwen/Qwen3-8B --gpus 1 \
  --dry-run client --output yaml > ./demo.yaml

airunway agent create assistant --framework langgraph --model-ref demo \
  --prompt "Be helpful." --dry-run server --output yaml

airunway apply --file ./demo.yaml
airunway apply --file ./deployments/ --dry-run server
```

Client dry-run does not contact the cluster or invoke authentication plugins.
Server dry-run performs admission without persisting resources. `apply` accepts
ModelDeployment and AgentDeployment documents, rejects cross-namespace surprises,
and does not force ownership conflicts or replace immutable resources. Parsing
all input documents happens before writes; subsequent API operations are not a
transaction. If a later operation fails, stdout lists the completed resources and
stderr reports that they were not rolled back.

```bash
airunway model create ci-demo --id hf://Qwen/Qwen3-8B --gpus 1 \
  --wait=false --output json
airunway model wait ci-demo --for ready --timeout 20m
airunway model chat ci-demo --message "Reply with OK." --output json
airunway model delete ci-demo
```

`--output json` puts results on stdout and structured errors/progress on stderr.
No command prompts when stdin is not a terminal. Creation/update waits by default;
`--wait=false` returns after submission. Timeouts and interruption leave submitted
resources available for inspection.

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | Operation failed |
| 2 | Invalid input or unsupported combination |
| 3 | Authentication, authorization, or connectivity failure |
| 4 | Timeout |
| 5 | Existing-name or concurrent-update conflict |
| 130 | Interrupted |

```bash
airunway provider list
airunway provider get vllm
airunway catalog model search qwen
airunway catalog model get hf://Qwen/Qwen3-8B
airunway completion zsh
airunway model create --help
airunway version
```

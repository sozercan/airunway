package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// runAgentOnce shares creation with agent create but never prints the resource
// as a task result. A run submits exactly once and never deletes its resources.
func runAgentOnce(words []string, c *CommandContext, config *CLIConfig, localPreview bool) error {
	flags, dry, err := agentRunFlags(words, c)
	if err != nil {
		return err
	}
	run := *c
	run.Flags = flags
	// One deadline covers submission, completion, and result retrieval. In
	// particular, a completed Job must not leave a blocked log read unbounded.
	ctx, cancel, err := accessDeadline(c.Context, run.Flags)
	if err != nil {
		return err
	}
	defer cancel()
	run.Context = ctx
	if ctx.Err() != nil {
		return accessContextError(ctx)
	}
	resource, err := createResource(resourceAgent, words[2], &run, config, localPreview, agentRunConfigureResult)
	if ctx.Err() != nil {
		return accessContextError(ctx)
	}
	if err != nil {
		return err
	}
	if dry != "" {
		return writeOutput(c.IO, c.Flags, resource)
	}
	client, err := run.Client()
	if err != nil {
		return err
	}
	progress(&run, fmt.Sprintf("Agent %q completed. Reading its task result; resources will not be deleted.", words[2]))
	result, err := agentRunResult(ctx, client, resource, run.Namespace)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return accessContextError(ctx)
	}
	return writeOutput(c.IO, c.Flags, result)
}

func agentRunFlags(words []string, c *CommandContext) (Flags, string, error) {
	if err := assertPositionals(words, 3); err != nil {
		return nil, "", err
	}
	if err := validateName(words[2], "name"); err != nil {
		return nil, "", err
	}
	if err := assertFlags(c.Flags, createOptions); err != nil {
		return nil, "", err
	}
	if c.Flags.Has("mode") && c.Flags.Text("mode") != "once" && c.Flags.Text("mode") != agentLifecycleJob {
		return nil, "", usage("agent run always runs a one-shot task. Use agent create for a long-running agent.")
	}
	dry, err := dryRun(c.Flags)
	if err != nil {
		return nil, "", err
	}
	if dry == "" && c.Flags.Has("wait") && !c.Flags.Bool("wait") {
		return nil, "", usage(
			"agent run waits for the task result. Use agent create --mode once --wait=false to submit " +
				"without waiting.",
		)
	}
	flags := c.Flags.Copy()
	flags["mode"] = []string{"once"}
	if dry == "" {
		flags["wait"] = []string{"true"}
	}
	return flags, dry, nil
}

func agentRunResult(ctx context.Context, client ClusterClient, resource Object, namespace string) (string, error) {
	ns := accessNamespace(resource, namespace)
	job, err := agentRunJob(ctx, client, resource, ns)
	if err != nil {
		return "", err
	}
	selected, err := agentRunSelectPod(ctx, client, job, ns)
	if err != nil {
		return "", err
	}
	result, err := agentRunReadResult(ctx, client, selected, ns)
	if err != nil {
		return "", err
	}
	// The logs API addresses a pod by name, not UID. Buffer the result until a
	// second read confirms the selected pod was not replaced while reading.
	current, err := accessGet(ctx, client, resourceTypes["pod"], ns, stringAt(selected, "metadata", "name"))
	if err != nil {
		return "", err
	}
	if stringAt(current, "metadata", "uid") != stringAt(selected, "metadata", "uid") ||
		stringAt(current, "metadata", "namespace") != ns || !agentRunPodOwnedByJob(current, job) {
		return "", cliError(1, "DELETED", "The task pod was replaced or its ownership changed while reading its result.")
	}
	return result, nil
}

func agentRunJob(ctx context.Context, client ClusterClient, resource Object, ns string) (Object, error) {
	if stringAt(resource, "metadata", "uid") == "" {
		return nil, accessUnsupported("The agent has no UID; task result ownership cannot be verified.")
	}
	ref := object(get(resource, "status", "runtime", "workloadRef"))
	if stringAt(ref, "kind") != agentJobKind || stringAt(ref, "apiVersion") != agentJobAPIVersion {
		return nil, accessUnsupported("The completed agent has not published a batch/v1 Job reference. Inspect its status.")
	}
	if publishedNS := stringAt(ref, "namespace"); publishedNS != "" && publishedNS != ns {
		return nil, accessUnsupported("The published Job namespace does not match the agent namespace.")
	}
	job, err := accessWorkload(ctx, client, resourceAgent, resource, ns)
	if err != nil {
		return nil, err
	}
	if stringAt(job, "metadata", "uid") == "" {
		return nil, accessUnsupported("The backing Job has no UID; task result ownership cannot be verified.")
	}
	owned, err := accessDescendsFrom(ctx, client, job, resource, map[string]Object{}, map[string]bool{}, ns)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, accessUnsupported(
			"The published Job does not belong to this agent UID. Refusing to read another task's result.",
		)
	}
	return job, nil
}

func agentRunSelectPod(ctx context.Context, client ClusterClient, job Object, ns string) (Object, error) {
	pods, err := accessList(ctx, client, resourceTypes["pod"], ns, nil)
	if err != nil {
		return nil, err
	}
	var selected Object
	for _, pod := range pods {
		if stringAt(pod, "metadata", "namespace") != ns || !agentRunSuccessfulPod(pod) {
			continue
		}
		if !agentRunPodOwnedByJob(pod, job) {
			continue
		}
		if stringAt(pod, "metadata", "uid") == "" {
			return nil, accessUnsupported("The successful pod has no UID; task result ownership cannot be verified.")
		}
		if selected != nil {
			return nil, cliError(1, "RESULT_UNAVAILABLE",
				"More than one successful pod belongs to this task. Its final result is ambiguous; inspect "+
					"the agent logs.",
			)
		}
		selected = pod
	}
	if selected == nil {
		return nil, cliError(1, "RESULT_UNAVAILABLE",
			"No successful agent container belonging to the completed Job was found. Inspect its logs and "+
				"events; the task was not resubmitted.",
		)
	}
	return selected, nil
}

// Kubernetes Jobs own their pods directly. Do not follow other pods' owners:
// listing this namespace does not imply permission to read unrelated Jobs.
func agentRunPodOwnedByJob(pod, job Object) bool {
	uid := stringAt(job, "metadata", "uid")
	if uid == "" {
		return false
	}
	for _, ref := range objects(get(pod, "metadata", "ownerReferences")) {
		if stringAt(ref, "uid") == uid && stringAt(ref, "kind") == agentJobKind &&
			stringAt(ref, "apiVersion") == agentJobAPIVersion &&
			stringAt(ref, "name") == stringAt(job, "metadata", "name") {
			return true
		}
	}
	return false
}

// Jobs can retry failed attempts. Only the successful agent container supplies
// the result, never an init container, sidecar, or a failed attempt's logs.
func agentRunSuccessfulPod(pod Object) bool {
	if stringAt(pod, "status", "phase") != "Succeeded" {
		return false
	}
	for _, container := range objects(get(pod, "spec", "containers")) {
		if stringAt(container, "name") != resourceAgent {
			continue
		}
		for _, status := range objects(get(pod, "status", "containerStatuses")) {
			if stringAt(status, "name") == resourceAgent && get(status, "state", "terminated", "exitCode") != nil {
				return intAt(status, "state", "terminated", "exitCode") == 0
			}
		}
	}
	return false
}

func agentRunReadResult(ctx context.Context, client ClusterClient, pod Object, namespace string) (string, error) {
	query := url.Values{"container": {resourceAgent}, "follow": {"false"}, "timestamps": {"false"}}
	// Do not tail the logs: diagnostics may follow the final-result record.
	response, err := accessCall(ctx, func() (*http.Response, error) {
		path := resourcePath(resourceTypes["pod"], namespace, stringAt(pod, "metadata", "name")) + "/log"
		response, err := client.Raw(ctx, http.MethodGet, path, nil, RequestOptions{Query: query})
		if response != nil && response.Body != nil {
			context.AfterFunc(ctx, func() { _ = response.Body.Close() })
		}
		return response, err
	})
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", cliError(1, "LOGS", "Reading the task result returned no response.")
	}
	if response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", cliError(1, "LOGS", fmt.Sprintf("Reading the task result failed with HTTP %d.", response.StatusCode))
	}
	if response.Body == nil {
		return "", agentRunNoResult()
	}
	return accessCall(ctx, func() (string, error) { return agentRunDecodeResult(response.Body) })
}

func agentRunNoResult() error {
	return cliError(1, "RESULT_UNAVAILABLE",
		"The task completed but its image did not emit a machine-readable final result. Raw pod logs "+
			"cannot reliably distinguish an answer from diagnostics. Inspect the logs; the task was not "+
			"resubmitted or deleted.",
	)
}

const agentResultFormat = "airunway-json-v1"
const agentJobKind = "Job"
const agentJobAPIVersion = "batch/v1"
const agentResultPrefix = "AIRUNWAY_RESULT_V1 "

func agentRunConfigureResult(resource Object) error {
	config := object(get(resource, "spec", keyConfig))
	if value, exists := config["resultFormat"]; exists && value != agentResultFormat {
		return usage("agent run requires config.resultFormat=airunway-json-v1. Remove the conflicting setting.")
	}
	if config == nil {
		config = Object{}
		object(resource["spec"])[keyConfig] = config
	}
	config["resultFormat"] = agentResultFormat
	return nil
}

// Only an explicit runtime record is an answer. Never guess from arbitrary
// combined stdout/stderr, which can contain framework and tool diagnostics.
func agentRunDecodeResult(reader io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxInput+1))
	if err != nil {
		return "", cliError(1, "RESULT_UNAVAILABLE", "Cannot read the task result. Inspect the agent logs.")
	}
	if len(data) > maxInput {
		return "", cliError(1, "RESULT_UNAVAILABLE",
			"Task logs exceed the 4 MiB result-reading limit. Inspect the agent logs.",
		)
	}
	result, found := "", false
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !bytes.HasPrefix(line, []byte(agentResultPrefix)) {
			continue
		}
		if found {
			return "", cliError(1, "RESULT_UNAVAILABLE", "The task emitted multiple final results. Inspect its logs.")
		}
		var record Object
		if decodeJSON(line[len(agentResultPrefix):], &record) != nil || len(record) != 1 {
			return "", agentRunNoResult()
		}
		output, ok := record["output"].(string)
		if !ok || strings.TrimSpace(output) == "" {
			return "", agentRunNoResult()
		}
		result, found = output, true
	}
	if !found {
		return "", agentRunNoResult()
	}
	return result, nil
}

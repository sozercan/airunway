package cli

import (
	"encoding/json"
	"fmt"
)

const (
	resourceAgent     = "agent"
	dryRunClient      = "client"
	dryRunServer      = "server"
	agentLifecycleJob = "job"
	keyConfig         = "config"
)

func createResource(
	noun, name string, c *CommandContext, config *CLIConfig, localPreview bool, prepare func(Object) error,
) (Object, error) {
	f := c.Flags
	dry, err := dryRun(f)
	if err != nil {
		return nil, err
	}
	creation, err := creationFlags(noun, c, config, localPreview)
	if err != nil {
		return nil, err
	}
	builder := buildModel
	if noun == resourceAgent {
		builder = buildAgent
	}
	// Input preparation may wait on a pipe or FIFO. Only this read-only step
	// runs in the bounded worker; preflight and submission stay on this caller.
	resource, err := accessCall(c.Context, func() (Object, error) {
		return builder(name, creation, c.Namespace, c.IO)
	})
	if err != nil {
		return nil, err
	}
	if prepare != nil {
		if err := prepare(resource); err != nil {
			return nil, err
		}
	}
	wait, err := writeWaitEnabled(noun, nil, resource, f, dry)
	if err != nil {
		return nil, err
	}
	if dry == dryRunClient {
		return resource, nil
	}
	client, err := c.Client()
	if err != nil {
		return nil, err
	}
	if err := preflight(resource, noun, c, client); err != nil {
		return nil, err
	}
	created, err := client.Create(c.Context, resource, dry == dryRunServer)
	if err != nil {
		return nil, err
	}
	if !wait {
		return created, nil
	}
	progress(c, fmt.Sprintf("Created %s %q in %s. Waiting; timeout or interruption will not delete it.",
		noun, name, c.Namespace))
	waitFlags := f.Copy()
	target := outputReady
	if stringAt(resource, "spec", "lifecycle") == agentLifecycleJob {
		target = outputCompleted
	}
	waitFlags["for"] = []string{target}
	ready, err := waitForResource(c.Context, client, noun, created, waitFlags, c.IO)
	if err != nil {
		return nil, err
	}
	return ready, nil
}

func creationFlags(noun string, c *CommandContext, config *CLIConfig, localPreview bool) (Flags, error) {
	f := c.Flags
	if noun != resourceAgent {
		return f, nil
	}
	creation := mergeAgentDefaults(f, effectiveDefaults(config, c.ContextName, c.Namespace))
	if !f.Has("preset") {
		return creation, nil
	}
	if localPreview {
		return nil, usage(
			"Preset resolution needs a cluster. Use --dry-run server or provide framework configuration directly.",
		)
	}
	client, err := c.Client()
	if err != nil {
		return nil, err
	}
	preset, err := resolvePreset(c.Context, client, f.Text("preset"))
	if err != nil {
		return nil, err
	}
	framework := stringAt(preset, "framework")
	if f.Has("framework") && f.Text("framework") != framework {
		return nil, usage("The preset and --framework disagree.")
	}
	creation["framework"] = []string{framework}
	b, _ := json.Marshal(preset[keyConfig])
	creation["__preset-config"] = []string{string(b)}
	return creation, nil
}

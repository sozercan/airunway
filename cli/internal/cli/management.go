package cli

import (
	"context"
	"regexp"
	"strings"
)

const (
	managementManagedBy      = "app.kubernetes.io/managed-by"
	managementCredentialType = "airunway.ai/credential-type"
	managementManager        = "airunway-cli"
)

var managementNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Management resources accept DNS subdomain names, unlike the shorter names
// accepted by the imperative deployment builders.
func managementIdentifier(value any, label string, max int) (string, error) {
	name, ok := value.(string)
	if !ok || len(name) > max || !managementNamePattern.MatchString(name) {
		return "", usage("Provide a valid " + label + ".")
	}
	return name, nil
}

func managementNamespace(ctx *CommandContext) (string, error) {
	ns, err := managementIdentifier(ctx.Namespace, "namespace", 63)
	if err != nil {
		return "", err
	}
	if strings.Contains(ns, ".") || ctx.Flags.Has("all-namespaces") {
		return "", usage("This command requires one namespace.")
	}
	return ns, nil
}

func managementOptions(ctx *CommandContext, allowed ...string) error {
	if err := assertFlags(ctx.Flags, allowed); err != nil {
		// Option names, as well as their values, can contain pasted credentials.
		return usage("Unsupported option for this command. Run with --help.")
	}
	switch ctx.Flags.Text("output") {
	case "", "text", "json", "yaml":
		return nil
	default:
		return usage("--output must be text, json, or yaml.")
	}
}

func managementArity(words []string, count int) error {
	if len(words) != count {
		return usage("Unexpected or missing command arguments. Run with --help.")
	}
	return nil
}

func managementDryRun(flags Flags) (string, error) {
	value := flags.Text("dry-run")
	if flags.Has("dry-run") && value != "client" && value != "server" {
		return "", usage("--dry-run must be client or server.")
	}
	return value, nil
}

func managementCanceled(ctx context.Context) error {
	if ctx.Err() != nil {
		return cliError(130, "INTERRUPTED", "Interrupted. Already submitted resources were not rolled back.")
	}
	return nil
}

func managementPick(value any, keys ...string) Object {
	result := Object{}
	for _, key := range keys {
		if v, ok := object(value)[key]; ok {
			result[key] = v
		}
	}
	return result
}

func managementDiscoveryMetadata(resource Object) Object {
	result := managementPick(resource, "apiVersion", "kind")
	result["metadata"] = managementPick(resource["metadata"], "name")
	result["spec"] = managementPick(resource["spec"], "capabilities", "selectionRules")
	result["status"] = managementPick(resource["status"], "ready", "version")
	return result
}

func managementDiscovery(words []string, ctx *CommandContext) error {
	if len(words) < 2 || (words[1] != "list" && words[1] != "get") {
		return usage("Use list or get for installed providers and frameworks.")
	}
	count := 2
	if words[1] == "get" {
		count = 3
	}
	if err := managementArity(words, count); err != nil {
		return err
	}
	if err := managementOptions(ctx); err != nil {
		return err
	}
	name := ""
	if words[1] == "get" {
		var err error
		name, err = managementIdentifier(words[2], "name", 253)
		if err != nil {
			return err
		}
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	client, err := ctx.Client()
	if err != nil {
		return err
	}
	t := resourceTypes[words[0]]
	if words[1] == "list" {
		items, err := client.List(ctx.Context, t, "", nil)
		if err != nil {
			return err
		}
		result := []Object{}
		for _, item := range items {
			result = append(result, managementDiscoveryMetadata(item))
		}
		return writeResourceOutput(ctx.IO, ctx.Flags, words[0], "list", result)
	}
	item, err := client.Get(ctx.Context, t, "", name)
	if err != nil {
		return err
	}
	return writeResourceOutput(ctx.IO, ctx.Flags, words[0], "get", managementDiscoveryMetadata(item))
}

func runManagement(words []string, ctx *CommandContext) (bool, error) {
	if len(words) == 0 {
		return false, nil
	}
	switch words[0] {
	case "credential":
		return true, managementCredential(words, ctx)
	case "provider", "framework":
		return true, managementDiscovery(words, ctx)
	case "apply":
		return true, managementApply(words, ctx)
	case "catalog":
		if err := managementOptions(ctx); err != nil {
			return true, err
		}
		if len(words) < 2 {
			return true, usage("Use catalog model or catalog agent.")
		}
		switch words[1] {
		case "model":
			return true, managementModelCatalog(words, ctx)
		case "agent":
			return true, managementAgentCatalog(words, ctx)
		default:
			return true, usage("Use catalog model or catalog agent.")
		}
	default:
		return false, nil
	}
}

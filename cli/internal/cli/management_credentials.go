package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strings"
	"unicode"
)

var managementCredentialKeys = map[string]string{"huggingface": "HF_TOKEN", "api-key": "API_KEY", "artifact": "credentials"}

func managementCredentialKind(value string) (string, error) {
	if _, ok := managementCredentialKeys[value]; !ok {
		return "", usage("--type must be huggingface, api-key, or artifact.")
	}
	return value, nil
}

func managementOwnedCredential(resource Object, ns string) (string, error) {
	if stringAt(resource, "kind") != "Secret" || stringAt(resource, "apiVersion") != "v1" ||
		stringAt(resource, "metadata", "namespace") != ns || stringAt(resource, "metadata", "labels", managementManagedBy) != managementManager ||
		stringAt(resource, "type") != "Opaque" || len(array(get(resource, "metadata", "ownerReferences"))) != 0 {
		return "", usage("This secret is not a CLI-managed credential in the selected namespace.")
	}
	return managementCredentialKind(stringAt(resource, "metadata", "labels", managementCredentialType))
}

// Never expose data, annotations, arbitrary labels, or server-generated fields.
func managementCredentialMetadata(resource Object) Object {
	metadata := managementPick(resource["metadata"], "name", "namespace", "uid", "resourceVersion", "creationTimestamp")
	metadata["labels"] = Object{managementManagedBy: managementManager, managementCredentialType: stringAt(resource, "metadata", "labels", managementCredentialType)}
	return Object{"apiVersion": "v1", "kind": "Secret", "metadata": metadata}
}

func managementReadCredential(ctx *CommandContext, kind string) (string, error) {
	path, err := required(ctx.Flags, "from-file")
	if err != nil {
		return "", err
	}
	limit, label := int64(maxInput), "4 MiB"
	if kind == "artifact" {
		limit, label = 64*1024, "64 KiB"
	}
	reader := ctx.IO.In
	if path != "-" {
		stat, err := os.Lstat(path)
		if err != nil {
			return "", usage("Cannot read credential input.")
		}
		if !stat.Mode().IsRegular() || stat.Size() > limit {
			return "", usage("Credential input must be a regular file of at most " + label + ".")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", usage("Cannot read credential input.")
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return "", usage("Cannot read credential input.")
	}
	if int64(len(data)) > limit {
		return "", usage("Credential input is limited to " + label + ".")
	}
	if kind == "artifact" {
		var value Object
		if err := json.Unmarshal(data, &value); err != nil || len(value) == 0 {
			return "", usage("Artifact credentials must be a nonempty JSON object with safe keys.")
		}
		if err := managementValidateJSON(value, false); err != nil {
			return "", usage("Artifact credentials must be a nonempty JSON object with safe keys.")
		}
		// Compact the original JSON to preserve source-specific numbers and strings.
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return "", usage("Artifact credentials must be a nonempty JSON object with safe keys.")
		}
		if compact.Len() > int(limit) {
			return "", usage("Artifact credentials are limited to " + label + ".")
		}
		return compact.String(), nil
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) != len(data) {
		value = strings.TrimSuffix(value, "\r")
	}
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' || r < 32 || r == 127 }) >= 0 {
		return "", usage("Credential input must contain one nonempty token without whitespace.")
	}
	return value, nil
}

func managementReferencesCredential(resource Object, credential string) bool {
	if stringAt(resource, "spec", "secrets", "huggingFaceToken") == credential {
		return true
	}
	var scan func(any) bool
	scan = func(value any) bool {
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				if scan(child) {
					return true
				}
			}
		case map[string]any:
			for key, child := range item {
				switch key {
				case "secretKeyRef", "secretRef", "credentialsRef", "authSecretRef":
					if stringAt(child, "name") == credential {
						return true
					}
				case "imagePullSecrets":
					for _, ref := range array(child) {
						if stringAt(ref, "name") == credential {
							return true
						}
					}
				}
				if scan(child) {
					return true
				}
			}
		}
		return false
	}
	return scan(resource["spec"]) || scan(get(resource, "status", "modelBinding"))
}

func managementCredential(words []string, ctx *CommandContext) error {
	if len(words) < 2 {
		return usage("Use credential create, list, get, update, or delete.")
	}
	action := words[1]
	count := 3
	switch action {
	case "list":
		count = 2
	case "create", "get", "update", "delete":
	default:
		return usage("Use credential create, list, get, update, or delete.")
	}
	if err := managementArity(words, count); err != nil {
		return err
	}
	allowed := []string{}
	if action == "create" || action == "update" {
		allowed = []string{"type", "from-file", "dry-run"}
	}
	if err := managementOptions(ctx, allowed...); err != nil {
		return err
	}
	ns, err := managementNamespace(ctx)
	if err != nil {
		return err
	}
	name := ""
	if action != "list" {
		name, err = managementIdentifier(words[2], "credential name", 253)
		if err != nil {
			return err
		}
	}
	dry, err := managementDryRun(ctx.Flags)
	if err != nil {
		return err
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	if action == "create" {
		kind, err := required(ctx.Flags, "type")
		if err != nil {
			return err
		}
		kind, err = managementCredentialKind(kind)
		if err != nil {
			return err
		}
		value, err := managementReadCredential(ctx, kind)
		if err != nil {
			return err
		}
		desired := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{
			"name": name, "namespace": ns, "labels": Object{managementManagedBy: managementManager, managementCredentialType: kind},
		}, "data": Object{managementCredentialKeys[kind]: base64.StdEncoding.EncodeToString([]byte(value))}}
		if err := managementCanceled(ctx.Context); err != nil {
			return err
		}
		result := desired
		if dry != "client" {
			client, err := ctx.Client()
			if err != nil {
				return err
			}
			result, err = client.Create(ctx.Context, desired, dry == "server")
			if err != nil {
				return err
			}
		}
		return writeOutput(ctx.IO, ctx.Flags, managementCredentialMetadata(result))
	}
	client, err := ctx.Client()
	if err != nil {
		return err
	}
	if action == "list" {
		items, err := client.List(ctx.Context, resourceTypes["credential"], ns, url.Values{"labelSelector": {managementManagedBy + "=" + managementManager + "," + managementCredentialType}})
		if err != nil {
			return err
		}
		result := []Object{}
		for _, item := range items {
			if _, err := managementOwnedCredential(item, ns); err == nil {
				result = append(result, managementCredentialMetadata(item))
			}
		}
		return writeOutput(ctx.IO, ctx.Flags, result)
	}
	existing, err := client.Get(ctx.Context, resourceTypes["credential"], ns, name)
	if err != nil {
		return err
	}
	kind, err := managementOwnedCredential(existing, ns)
	if err != nil {
		return err
	}
	if action == "get" {
		return writeOutput(ctx.IO, ctx.Flags, managementCredentialMetadata(existing))
	}
	if action == "update" {
		if ctx.Flags.Has("type") {
			requested, err := managementCredentialKind(ctx.Flags.Text("type"))
			if err != nil {
				return err
			}
			if requested != kind {
				return usage("Credential type cannot change. Create a new credential instead.")
			}
		}
		version := stringAt(existing, "metadata", "resourceVersion")
		if version == "" {
			return usage("Cannot update a credential without its resourceVersion.")
		}
		value, err := managementReadCredential(ctx, kind)
		if err != nil {
			return err
		}
		if err := managementCanceled(ctx.Context); err != nil {
			return err
		}
		patch := Object{"metadata": Object{"resourceVersion": version}, "data": Object{managementCredentialKeys[kind]: base64.StdEncoding.EncodeToString([]byte(value))}}
		result := existing
		if dry != "client" {
			result, err = client.Patch(ctx.Context, resourceTypes["credential"], ns, name, patch, dry == "server")
			if err != nil {
				return err
			}
		}
		return writeOutput(ctx.IO, ctx.Flags, managementCredentialMetadata(result))
	}
	uid := stringAt(existing, "metadata", "uid")
	if uid == "" {
		return usage("Cannot delete a credential without its UID.")
	}
	// A missing API or denied list is not proof that the credential is unused.
	for _, noun := range []string{"model", "agent"} {
		items, err := client.List(ctx.Context, resourceTypes[noun], ns, nil)
		if err != nil {
			return err
		}
		for _, item := range items {
			if stringAt(item, "metadata", "namespace") == ns && managementReferencesCredential(item, name) {
				return cliError(5, "IN_USE", "Credential is referenced by a deployment in this namespace. Remove the reference before deleting it.")
			}
		}
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	if err := client.Delete(ctx.Context, resourceTypes["credential"], ns, name, uid); err != nil {
		return err
	}
	return writeOutput(ctx.IO, ctx.Flags, managementCredentialMetadata(existing))
}

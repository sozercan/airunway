package cli

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"unicode"
)

const (
	outputWait      = "wait"
	outputReady     = "ready"
	outputCompleted = "completed"
)

func textOutput(flags Flags) bool {
	return flags.Text("output") == "" || flags.Text("output") == "text"
}

// Terminal summaries are deliberately separate from machine-readable objects.
func writeResourceOutput(streams *IO, flags Flags, noun, action string, value any) error {
	if !textOutput(flags) || flags.Has("dry-run") {
		return writeOutput(streams, flags, value)
	}
	if action == "list" {
		rows, ok := value.([]Object)
		if !ok {
			rows = objects(value)
		}
		return writeResourceTable(streams, noun, rows)
	}
	resource := object(value)
	if action == "get" {
		return writeResourceSummary(streams, noun, resource)
	}
	name := stringAt(resource, "metadata", "name")
	if name == "" {
		name = stringAt(resource, "name")
	}
	if action == outputWait {
		state := outputReady
		if flags.Text("for") == outputCompleted {
			state = outputCompleted
		}
		return writeReceipt(streams, flags, textCell(name)+" "+state, resourceBrief(noun, resource))
	}
	if action == "update" {
		action = "updated"
	}
	return writeReceipt(streams, flags, noun+" "+textCell(name)+" "+action, resourceBrief(noun, resource))
}

func textCell(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func firstText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func resourcePhase(resource Object) string {
	phase := stringAt(resource, "status", "phase")
	generation, observed := intAt(resource, "metadata", "generation"), intAt(resource, "status", "observedGeneration")
	if generation > 0 && observed < generation {
		if phase == "" {
			return "Pending"
		}
		return phase + " (updating)"
	}
	return phase
}

func resourceProvider(resource Object) string {
	return firstText(stringAt(resource, "status", "provider", "name"), stringAt(resource, "spec", "provider", "name"))
}

func resourceFramework(resource Object) string {
	return firstText(stringAt(resource, "status", "framework", "name"), stringAt(resource, "spec", "framework", "name"))
}

func resourceReplicas(resource Object) string {
	if stringAt(resource, "spec", "lifecycle") == agentLifecycleJob {
		return ""
	}
	if len(object(get(resource, "status", "replicas"))) == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", intAt(resource, "status", "replicas", outputReady),
		intAt(resource, "status", "replicas", "desired"))
}

func resourceModelBinding(resource Object) string {
	ref := object(get(resource, "spec", "model", "deploymentRef"))
	if name := stringAt(ref, "name"); name != "" {
		if ns := stringAt(ref, "namespace"); ns != "" {
			return ns + "/" + name
		}
		return name
	}
	return firstText(stringAt(resource, "status", "modelBinding", "modelName"),
		stringAt(resource, "spec", "model", "externalAPI", "modelName"),
		stringAt(resource, "spec", "model", "gatewayEndpoint", "modelName"))
}

func resourceBrief(noun string, resource Object) string {
	parts := []string{}
	if phase := resourcePhase(resource); phase != "" {
		parts = append(parts, phase)
	}
	provider := resourceProvider(resource)
	if noun == resourceAgent {
		provider = resourceFramework(resource)
	}
	if provider != "" {
		parts = append(parts, provider)
	}
	if replicas := resourceReplicas(resource); replicas != "" {
		parts = append(parts, replicas+" replicas ready")
	}
	if len(parts) == 0 {
		return ""
	}
	return textCell(strings.Join(parts, ", "))
}

func resourceReadiness(resource Object) string {
	if get(resource, "status", outputReady) == nil {
		return "Unknown"
	}
	if boolAt(resource, "status", outputReady) {
		return "Ready"
	}
	return "Not ready"
}

func resourceEngines(resource Object) string {
	engines := []string{}
	for _, engine := range objects(get(resource, "spec", "capabilities", "engines")) {
		if name := stringAt(engine, "name"); name != "" {
			engines = append(engines, name)
		}
	}
	return strings.Join(engines, ",")
}

func resourceColumns(noun string, resource Object) []string {
	base := []string{stringAt(resource, "metadata", "name"),
		stringAt(resource, "metadata", "namespace"), resourcePhase(resource)}
	switch noun {
	case "model":
		return append(base, resourceProvider(resource), firstText(stringAt(resource, "status", "engine", "type"),
			stringAt(resource, "spec", "engine", "type")), resourceReplicas(resource))
	case resourceAgent:
		return append(base, resourceFramework(resource), resourceModelBinding(resource), resourceReplicas(resource))
	case "provider":
		return []string{base[0], resourceReadiness(resource), resourceEngines(resource),
			stringAt(resource, "status", "version")}
	case "framework":
		return []string{base[0], resourceReadiness(resource), stringAt(resource, "spec", "capabilities", "backend"),
			stringAt(resource, "status", "version")}
	case "credential":
		return []string{base[0], base[1], stringAt(resource, "metadata", "labels", managementCredentialType)}
	}
	return base
}

func writeTextTable(streams *IO, rows [][]string) error {
	writer := tabwriter.NewWriter(streams.Out, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, value := range row {
			cells[i] = textCell(value)
		}
		if _, err := fmt.Fprintln(writer, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func writeResourceTable(streams *IO, noun string, resources []Object) error {
	if len(resources) == 0 {
		_, err := fmt.Fprintln(streams.Out, "No results.")
		return err
	}
	headers := map[string][]string{
		"model":       {"NAME", "NAMESPACE", "STATUS", "PROVIDER", "ENGINE", "READY"},
		resourceAgent: {"NAME", "NAMESPACE", "STATUS", "FRAMEWORK", "MODEL", "READY"},
		"provider":    {"NAME", "STATUS", "ENGINES", "VERSION"},
		"framework":   {"NAME", "STATUS", "BACKEND", "VERSION"},
		"credential":  {"NAME", "NAMESPACE", "TYPE"},
	}
	rows := [][]string{headers[noun]}
	for _, resource := range resources {
		rows = append(rows, resourceColumns(noun, resource))
	}
	return writeTextTable(streams, rows)
}

func summaryEndpoint(resource Object) string {
	address := firstText(stringAt(resource, "status", "gateway", "endpoint"),
		stringAt(resource, "status", "runtime", "address"))
	if address != "" {
		u, err := url.Parse(address)
		if err == nil && u.Host != "" {
			u.User, u.RawQuery, u.Fragment = nil, "", ""
			return u.String()
		}
		return ""
	}
	service := stringAt(resource, "status", "endpoint", "service")
	if service != "" {
		return fmt.Sprintf("%s:%d", service, intAt(resource, "status", "endpoint", "port"))
	}
	return ""
}

func writeResourceSummary(streams *IO, noun string, resource Object) error {
	columns := resourceColumns(noun, resource)
	if noun == "provider" || noun == "framework" || noun == "credential" {
		return writeResourceTable(streams, noun, []Object{resource})
	}
	providerLabel, model := "Provider:", stringAt(resource, "spec", "model", "id")
	if noun == resourceAgent {
		providerLabel, model = "Framework:", resourceModelBinding(resource)
	}
	rows := [][]string{{"Name:", columns[0]}, {"Namespace:", columns[1]}, {"Status:", columns[2]},
		{providerLabel, columns[3]}, {"Model:", model}}
	if stringAt(resource, "spec", "lifecycle") == agentLifecycleJob {
		rows = append(rows, []string{"Mode:", "once"})
	} else {
		rows = append(rows, []string{"Replicas ready:", resourceReplicas(resource)})
	}
	if noun == "model" {
		rows = append(rows, []string{"Engine:", columns[4]})
		rows = append(rows, planRows(resource)...)
	}
	if endpoint := summaryEndpoint(resource); endpoint != "" {
		rows = append(rows, []string{"Endpoint:", endpoint})
	}
	for _, condition := range objects(get(resource, "status", "conditions")) {
		// A gateway the user turned off is a setting, not a problem to report.
		if stringAt(condition, "type") == "GatewayReady" && stringAt(condition, "reason") == "GatewayDisabled" {
			continue
		}
		if stringAt(condition, "status") == "False" {
			rows = append(rows, []string{stringAt(condition, "type") + ":", stringAt(condition, "reason")})
		}
	}
	return writeTextTable(streams, rows)
}

// planRows shows the serving plan a provider reports in status, such as the
// layout Dynamo selected during automatic configuration. Values are what the
// plan states, not measured pods or GPUs; anything it omits stays omitted.
func planRows(resource Object) [][]string {
	plan := object(get(resource, "status", "provider", "intent", "plan"))
	if len(plan) == 0 {
		return nil
	}
	mode := stringAt(plan, "servingMode")
	if mode == "" {
		mode = "layout not reported"
	}
	origin := "selected by Dynamo"
	if stringAt(plan, "source") == "workload" {
		origin = "read from the serving workload"
	}
	rows := [][]string{{"Plan:", mode + ", " + origin}}
	for _, worker := range objects(get(plan, "workers")) {
		label := "Worker:"
		if role := stringAt(worker, "role"); role == "prefill" || role == "decode" {
			label = strings.ToUpper(role[:1]) + role[1:] + " worker:"
		}
		parts := []string{textCell(stringAt(worker, "name"))}
		replicas := int64(-1)
		if get(worker, "replicas") != nil {
			replicas = intAt(worker, "replicas")
			parts = append(parts, countText(replicas, "replica"))
		}
		if get(worker, "gpusPerReplica") != nil {
			gpus := countText(intAt(worker, "gpusPerReplica"), "GPU")
			if replicas > 1 {
				gpus += " each"
			}
			parts = append(parts, gpus)
		}
		if tp := intAt(worker, "tensorParallelism"); tp > 1 {
			parts = append(parts, fmt.Sprintf("tensor parallel %d", tp))
		}
		if pp := intAt(worker, "pipelineParallelism"); pp > 1 {
			parts = append(parts, fmt.Sprintf("pipeline parallel %d", pp))
		}
		rows = append(rows, []string{label, strings.Join(parts, ", ")})
	}
	return rows
}

func countText(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func writeApplyOutput(streams *IO, flags Flags, resources []Object) error {
	if !textOutput(flags) || flags.Text("dry-run") == dryRunClient {
		return writeOutput(streams, flags, resources)
	}
	action := "applied"
	if flags.Text("dry-run") == dryRunServer {
		action = "validated (dry run)"
	}
	for _, resource := range resources {
		line := strings.ToLower(stringAt(resource, "kind")) + "/" + stringAt(resource, "metadata", "name") + " " + action
		if err := writeOutput(streams, flags, textCell(line)); err != nil {
			return err
		}
	}
	return nil
}

func writeDoctorOutput(c *CommandContext, checks []Object) error {
	result := Object{"context": c.ContextName, "namespace": c.Namespace, "checks": checks}
	if !textOutput(c.Flags) {
		return writeOutput(c.IO, c.Flags, result)
	}
	passed := 0
	for _, check := range checks {
		if boolAt(check, "ok") {
			passed++
		}
	}
	if _, err := fmt.Fprintf(c.IO.Out, "%d/%d access checks passed (context: %s, namespace: %s).\n",
		passed, len(checks), textCell(c.ContextName), textCell(c.Namespace)); err != nil {
		return err
	}
	for _, check := range checks {
		if boolAt(check, "ok") {
			continue
		}
		if _, err := fmt.Fprintf(c.IO.Out, "FAIL %s: %s\n", textCell(stringAt(check, "check")),
			textCell(stringAt(check, "detail"))); err != nil {
			return err
		}
	}
	if passed != len(checks) {
		_, err := fmt.Fprintln(c.IO.Out, "Check the selected context, installed APIs, and your access permissions.")
		return err
	}
	return nil
}

func writeReceipt(streams *IO, flags Flags, receipt, detail string) error {
	if detail != "" {
		receipt += ": " + detail
	}
	return writeOutput(streams, flags, receipt)
}

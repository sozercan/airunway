package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const maxInput = 4 * 1024 * 1024

func readInput(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, usage("Cannot read input.")
	}
	if int64(len(b)) > limit {
		return nil, usage("Input exceeds its size limit.")
	}
	return b, nil
}
func readFlagInput(f Flags, inline, file string, streams *IO) (string, bool, error) {
	if f.Has(inline) && f.Has(file) {
		return "", false, usage("Use --" + inline + " or --" + file + ", not both.")
	}
	if f.Has(inline) {
		return f.Text(inline), true, nil
	}
	if !f.Has(file) {
		return "", false, nil
	}
	var r io.Reader = streams.In
	if f.Text(file) != "-" {
		handle, err := os.Open(f.Text(file))
		if err != nil {
			return "", false, usage("Cannot read --" + file + " file.")
		}
		defer handle.Close()
		r = handle
	}
	b, err := readInput(r, maxInput)
	return string(b), true, err
}
func writeOutput(streams *IO, f Flags, v any) error {
	var b []byte
	var err error
	switch f.Text("output") {
	case "json":
		b, err = json.MarshalIndent(v, "", "  ")
	case "yaml":
		b, err = yaml.Marshal(v)
	case "", "text":
		if s, ok := v.(string); ok {
			_, err = fmt.Fprintln(streams.Out, strings.TrimSuffix(s, "\n"))
			return err
		}
		var rows []Object
		switch values := v.(type) {
		case []Object:
			rows = values
		case []any:
			rows = make([]Object, 0, len(values))
			for _, value := range values {
				rows = append(rows, object(value))
			}
		}
		if rows != nil {
			if len(rows) == 0 {
				_, err = fmt.Fprintln(streams.Out, "No results.")
				return err
			}
			for _, row := range rows {
				if metadata, ok := row["metadata"]; ok {
					ns := stringAt(metadata, "namespace")
					if ns == "" {
						ns = "-"
					}
					status := stringAt(row, "status", "phase")
					if status == "" && get(row, "status", "ready") != nil {
						if boolAt(row, "status", "ready") {
							status = "Ready"
						} else {
							status = "Not ready"
						}
					}
					_, err = fmt.Fprintf(streams.Out, "%s\t%s\t%s\n", stringAt(metadata, "name"), ns, status)
				} else {
					keys := make([]string, 0, len(row))
					for key := range row {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					parts := make([]string, 0, len(keys))
					for _, key := range keys {
						val := row[key]
						text := fmt.Sprint(val)
						switch val.(type) {
						case map[string]any, []any:
							data, _ := json.Marshal(val)
							text = string(data)
						}
						parts = append(parts, key+"="+text)
					}
					_, err = fmt.Fprintln(streams.Out, strings.Join(parts, "  "))
				}
				if err != nil {
					return err
				}
			}
			return nil
		}
		b, err = yaml.Marshal(v)
	default:
		return usage("--output must be text, json, or yaml.")
	}
	if err != nil {
		return cliError(1, "OUTPUT", "Cannot format command output.")
	}
	_, err = fmt.Fprintln(streams.Out, strings.TrimSuffix(string(b), "\n"))
	return err
}
func progress(c *CommandContext, message string) {
	if c.Flags.Text("output") == "json" {
		_ = json.NewEncoder(c.IO.Err).Encode(Object{"progress": Object{"message": strings.TrimSpace(message)}})
		return
	}
	_, _ = fmt.Fprintln(c.IO.Err, message)
}

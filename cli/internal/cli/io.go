package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

const maxInput = 4 * 1024 * 1024

// decodeJSON preserves numbers in untyped configuration and rejects trailing
// values just as json.Unmarshal does. Callers retain their own safe error text.
func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("expected a single JSON value")
	}
	return nil
}

// Marshal through JSON to retain JSON field names and custom marshalers, then
// emit numeric YAML nodes without the float conversion used by JSONToYAML.
func marshalYAML(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := decodeJSON(data, &decoded); err != nil {
		return nil, err
	}
	var toNode func(any) (*yaml.Node, error)
	toNode = func(value any) (*yaml.Node, error) {
		node := &yaml.Node{}
		switch value := value.(type) {
		case json.Number:
			node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!int", value.String()
			if strings.ContainsAny(value.String(), ".eE") {
				node.Tag = "!!float"
			}
		case map[string]any:
			node.Kind, node.Tag = yaml.MappingNode, "!!map"
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				keyNode, err := toNode(key)
				if err != nil {
					return nil, err
				}
				child, err := toNode(value[key])
				if err != nil {
					return nil, err
				}
				node.Content = append(node.Content, keyNode, child)
			}
		case []any:
			node.Kind, node.Tag = yaml.SequenceNode, "!!seq"
			for _, value := range value {
				child, err := toNode(value)
				if err != nil {
					return nil, err
				}
				node.Content = append(node.Content, child)
			}
		default:
			if err := node.Encode(value); err != nil {
				return nil, err
			}
		}
		return node, nil
	}
	node, err := toNode(decoded)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	encoder.CompactSeqIndent()
	if err := encoder.Encode(node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

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
		b, err = marshalYAML(v)
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
		b, err = marshalYAML(v)
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

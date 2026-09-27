package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

const precisionJSONManifest = `{
  "apiVersion":"airunway.ai/v1alpha1",
  "kind":"ModelDeployment",
  "metadata":{"name":"precise","namespace":"team"},
  "spec":{"model":{"id":"org/model"},"futureField":{
    "seed":9007199254740993,
    "negative":-9007199254740993,
    "unsigned":18446744073709551615,
    "huge":340282366920938463463374607431768211457,
    "decimalSeed":9007199254740993.0,
    "exponentSeed":9.007199254740993e15,
    "nested":[{"seed":9007199254740993}],
    "text":"9007199254740993"
  }}
}`

const precisionYAMLManifest = `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata: {name: precise, namespace: team}
spec:
  model: {id: org/model}
  futureField:
    seed: 9007199254740993
    negative: -9007199254740993
    unsigned: 18446744073709551615
    huge: 340282366920938463463374607431768211457
    decimalSeed: 9007199254740993.0
    exponentSeed: 9.007199254740993e15
    nested: [{seed: 9007199254740993}]
    text: "9007199254740993"
`

var precisionExpected = map[string]string{
	"seed": "9007199254740993", "negative": "-9007199254740993",
	"unsigned": "18446744073709551615", "huge": "340282366920938463463374607431768211457",
	"decimalSeed": "9007199254740993", "exponentSeed": "9007199254740993",
}

// Keep the test oracle independent of the production JSON decoder.
func precisionReadJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("expected one JSON value: %v", err)
	}
	return value
}

func precisionEqualNumber(t *testing.T, value any, expected string) {
	t.Helper()
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("got %T(%v), want exact JSON number %s", value, value, expected)
	}
	got, valid := new(big.Rat).SetString(number.String())
	want, _ := new(big.Rat).SetString(expected)
	if !valid || got.Cmp(want) != 0 {
		t.Fatalf("number changed: got %s, want %s", number, expected)
	}
}

func precisionCheckResource(t *testing.T, value any) {
	t.Helper()
	fields := object(get(value, "spec", "futureField"))
	for key, expected := range precisionExpected {
		precisionEqualNumber(t, fields[key], expected)
	}
	nested := array(fields["nested"])
	if len(nested) != 1 {
		t.Fatal("missing nested value")
	}
	precisionEqualNumber(t, get(nested[0], "seed"), precisionExpected["seed"])
	if fields["text"] != precisionExpected["seed"] {
		t.Fatal("numeric-looking string changed")
	}
}

func precisionYAMLAt(t *testing.T, node *yaml.Node, path ...string) *yaml.Node {
	t.Helper()
	if node.Kind == yaml.DocumentNode {
		node = node.Content[0]
	}
	for _, key := range path {
		var found *yaml.Node
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == key {
					found = node.Content[i+1]
					break
				}
			}
		}
		if found == nil {
			t.Fatalf("missing YAML field %s", strings.Join(path, "."))
		}
		node = found
	}
	return node
}

func precisionCheckYAMLResource(t *testing.T, node *yaml.Node) {
	t.Helper()
	for key, expected := range precisionExpected {
		number := precisionYAMLAt(t, node, "spec", "futureField", key)
		if number.Tag != "!!int" && number.Tag != "!!float" {
			t.Fatalf("YAML number %s became %s: %s", key, number.Tag, number.Value)
		}
		precisionEqualNumber(t, json.Number(number.Value), expected)
	}
	text := precisionYAMLAt(t, node, "spec", "futureField", "text")
	if text.Tag != "!!str" || text.Value != precisionExpected["seed"] {
		t.Fatalf("numeric-looking string changed: %#v", text)
	}
}

func TestPrecisionDeclarativePreviews(t *testing.T) {
	for _, input := range []struct{ ext, data string }{{"json", precisionJSONManifest}, {"yaml", precisionYAMLManifest}} {
		for _, output := range []string{"json", "yaml"} {
			t.Run(input.ext+"/"+output, func(t *testing.T) {
				path := managementTestWrite(t, t.TempDir(), "precise."+input.ext, input.data)
				h := managementTestContext("file", path, "dry-run", "client", "output", output)
				managementTestRun(t, h, "apply")
				if h.connections != 0 {
					t.Fatal("client preview contacted cluster")
				}
				if output == "json" {
					values := array(precisionReadJSON(t, h.out.Bytes()))
					if len(values) != 1 {
						t.Fatalf("unexpected preview: %s", h.out.String())
					}
					precisionCheckResource(t, values[0])
				} else {
					var node yaml.Node
					if err := yaml.Unmarshal(h.out.Bytes(), &node); err != nil {
						t.Fatal(err)
					}
					items := node.Content[0]
					if items.Kind != yaml.SequenceNode || len(items.Content) != 1 {
						t.Fatalf("unexpected preview: %s", h.out.String())
					}
					precisionCheckYAMLResource(t, items.Content[0])
				}
			})
		}
	}
}

func TestPrecisionCloneAndOutput(t *testing.T) {
	original := object(precisionReadJSON(t, []byte(precisionJSONManifest)))
	fields := object(get(original, "spec", "futureField"))
	fields["signedGoInteger"] = int64(9007199254740993)
	fields["unsignedGoInteger"] = ^uint64(0)
	fields["raw"] = json.RawMessage(`{"seed":9007199254740993}`)
	cloned := cloneObject(original)
	precisionCheckResource(t, cloned)
	precisionEqualNumber(t, get(cloned, "spec", "futureField", "signedGoInteger"), "9007199254740993")
	precisionEqualNumber(t, get(cloned, "spec", "futureField", "unsignedGoInteger"), "18446744073709551615")
	precisionEqualNumber(t, get(cloned, "spec", "futureField", "raw", "seed"), "9007199254740993")
	object(get(cloned, "spec", "futureField"))["text"] = "changed"
	if fields["text"] != precisionExpected["seed"] {
		t.Fatal("clone shares mutable maps with source")
	}
	for _, format := range []string{"json", "yaml", "text"} {
		t.Run(format, func(t *testing.T) {
			h := managementTestContext("output", format)
			if err := writeOutput(h.ctx.IO, h.ctx.Flags, original); err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				precisionCheckResource(t, precisionReadJSON(t, h.out.Bytes()))
			} else {
				var node yaml.Node
				if err := yaml.Unmarshal(h.out.Bytes(), &node); err != nil {
					t.Fatal(err)
				}
				precisionCheckYAMLResource(t, &node)
			}
		})
	}
}

func TestPrecisionApplyAPIBody(t *testing.T) {
	for _, input := range []struct{ ext, data string }{{"json", precisionJSONManifest}, {"yaml", precisionYAMLManifest}} {
		for _, dry := range []string{"", "server"} {
			t.Run(input.ext+"/"+dry, func(t *testing.T) {
				requests := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPatch || r.URL.Path != "/apis/airunway.ai/v1alpha1/namespaces/team/modeldeployments/precise" || r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
						t.Errorf("unexpected apply request: %s %s %s", r.Method, r.URL, r.Header.Get("Content-Type"))
					}
					if (r.URL.Query().Get("dryRun") == "All") != (dry == "server") || r.URL.Query().Get("force") != "false" {
						t.Errorf("incorrect apply flags: %s", r.URL)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					requests <- body
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				}))
				defer server.Close()
				client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "team")
				if err != nil {
					t.Fatal(err)
				}
				path := managementTestWrite(t, t.TempDir(), "precise."+input.ext, input.data)
				h := managementTestContext("file", path)
				if dry != "" {
					h.ctx.Flags["dry-run"] = []string{dry}
				}
				h.ctx.Client = func() (ClusterClient, error) { return client, nil }
				managementTestRun(t, h, "apply")
				precisionCheckResource(t, precisionReadJSON(t, <-requests))
			})
		}
	}
}

func TestPrecisionClusterResponseAndRequest(t *testing.T) {
	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, precisionJSONManifest)
	}))
	defer server.Close()
	client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "team")
	if err != nil {
		t.Fatal(err)
	}
	body := object(precisionReadJSON(t, []byte(precisionJSONManifest)))
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			var request any
			if method != http.MethodGet {
				request = cloneObject(body)
			}
			result, err := client.Request(context.Background(), method, "/precision", request, RequestOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data := <-requests
			precisionCheckResource(t, result)
			if request != nil {
				precisionCheckResource(t, precisionReadJSON(t, data))
			}
			if got := intAt(result, "spec", "futureField", "seed"); got != 9007199254740993 {
				t.Fatalf("intAt rounded number: %d", got)
			}
		})
	}
}

func TestPrecisionApplyRejectsTrailingJSON(t *testing.T) {
	for _, trailing := range []string{"{}", "null", "garbage"} {
		path := managementTestWrite(t, t.TempDir(), "precise.json", precisionJSONManifest+trailing)
		h := managementTestContext("file", path, "dry-run", "client")
		managementTestError(t, h, "Cannot parse apply input", "apply")
		if h.connections != 0 {
			t.Fatal("invalid JSON reached cluster")
		}
	}
}

func TestPrecisionDecodeJSONStrict(t *testing.T) {
	for _, input := range []string{
		`9007199254740993`, `-9007199254740993`, `18446744073709551615`,
		`340282366920938463463374607431768211457`, `9007199254740993.0`, `9.007199254740993e15`,
	} {
		var value any
		if err := decodeJSON([]byte(" \n"+input+"\t\r\n"), &value); err != nil {
			t.Fatal(err)
		}
		number, ok := value.(json.Number)
		if !ok || number.String() != input {
			t.Fatalf("decode lost spelling or precision: %T(%v), expected %s", value, value, input)
		}
	}
	for _, input := range []string{"", " ", "{}{}", "{} null", "[] []", "1 2", `{} trailing`, `{"x":1`, `01`, `NaN`, `Infinity`} {
		var value any
		if err := decodeJSON([]byte(input), &value); err == nil {
			t.Fatalf("accepted invalid or multiple JSON values %q", input)
		}
	}
	var typed struct {
		Seed   int64 `json:"seed"`
		Nested any   `json:"nested"`
	}
	if err := decodeJSON([]byte(`{"seed":9007199254740993,"nested":{"seed":9007199254740993}}`), &typed); err != nil {
		t.Fatal(err)
	}
	if typed.Seed != 9007199254740993 {
		t.Fatalf("typed integer changed: %d", typed.Seed)
	}
	precisionEqualNumber(t, get(typed.Nested, "seed"), "9007199254740993")
	var null any = "not null"
	if err := decodeJSON([]byte("null"), &null); err != nil || null != nil {
		t.Fatalf("null contract changed: %v %v", null, err)
	}
}

func TestPrecisionYAMLNumericForms(t *testing.T) {
	integer := "340282366920938463463374607431768211457"
	bigInteger, _ := new(big.Int).SetString(integer, 10)
	cases := []struct{ input, expected string }{
		{"+09007199254740993", "9007199254740993"},
		{"-09007199254740993", "-9007199254740993"},
		{"0x" + bigInteger.Text(16), integer},
		{"0o" + bigInteger.Text(8), integer},
		{"!!int 0b" + bigInteger.Text(2), integer},
		{"!!int -0x" + bigInteger.Text(16), "-" + integer},
		{"0009007199254740993.", "9007199254740993"},
		{"+0009007199254740993.0e+0", "9007199254740993"},
		{".9007199254740993e16", "9007199254740993"},
		{"-.9007199254740993e16", "-9007199254740993"},
		{"!!float 9007199254740993", "9007199254740993"},
		{"1.234567890123456789", "1.234567890123456789"},
		{"-0.0", "0"},
	}
	for _, test := range cases {
		t.Run(test.input, func(t *testing.T) {
			var node yaml.Node
			if err := yaml.Unmarshal([]byte(test.input), &node); err != nil {
				t.Fatal(err)
			}
			value, err := managementYAMLDocument(&node)
			if err != nil {
				t.Fatal(err)
			}
			precisionEqualNumber(t, value, test.expected)
			for _, format := range []string{"json", "yaml"} {
				h := managementTestContext("output", format)
				if err := writeOutput(h.ctx.IO, h.ctx.Flags, Object{"value": value}); err != nil {
					t.Fatal(err)
				}
				if format == "json" {
					precisionEqualNumber(t, get(precisionReadJSON(t, h.out.Bytes()), "value"), test.expected)
				} else {
					var node yaml.Node
					if err := yaml.Unmarshal(h.out.Bytes(), &node); err != nil {
						t.Fatal(err)
					}
					number := precisionYAMLAt(t, &node, "value")
					if number.Tag != "!!int" && number.Tag != "!!float" {
						t.Fatal("numeric YAML emitted as a string")
					}
					precisionEqualNumber(t, json.Number(number.Value), test.expected)
				}
			}
		})
	}
}

func TestPrecisionYAMLPreviewReapply(t *testing.T) {
	original := object(precisionReadJSON(t, []byte(precisionJSONManifest)))
	data, err := marshalYAML(original)
	if err != nil {
		t.Fatal(err)
	}
	path := managementTestWrite(t, t.TempDir(), "precise.yaml", string(data))
	h := managementTestContext("file", path, "dry-run", "client", "output", "json")
	managementTestRun(t, h, "apply")
	items := array(precisionReadJSON(t, h.out.Bytes()))
	if len(items) != 1 {
		t.Fatal("missing preview result")
	}
	precisionCheckResource(t, items[0])
}

func TestPrecisionClusterListAndInvalidJSON(t *testing.T) {
	for _, response := range []string{
		`{"items":[` + precisionJSONManifest + `]}`, precisionJSONManifest + `{}`, precisionJSONManifest + ` trailing`,
		precisionJSONManifest + ` null`, `null`, `[]`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}))
		client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "team")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		items, err := client.List(context.Background(), resourceTypes["model"], "team", nil)
		server.Close()
		if strings.HasPrefix(response, `{"items"`) {
			if err != nil || len(items) != 1 {
				t.Fatalf("list failed: %v", err)
			}
			precisionCheckResource(t, items[0])
		} else {
			managementTestCode(t, err, "RESPONSE")
		}
	}
}

func TestPrecisionYAMLJSONContracts(t *testing.T) {
	value := struct {
		Number  json.Number     `json:"exact_id" yaml:"wrong_key"`
		Raw     json.RawMessage `json:"custom"`
		Hidden  string          `json:"-"`
		Empty   string          `json:"omitted,omitempty"`
		Null    any             `json:"null_value"`
		Enabled bool            `json:"enabled"`
		Strings []string        `json:"strings"`
	}{Number: json.Number("9007199254740993"), Raw: json.RawMessage(`{"seed":340282366920938463463374607431768211457}`), Hidden: "private", Enabled: true, Strings: []string{"yes", "null", "1e3", "9007199254740993"}}
	data, err := marshalYAML(value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("wrong_key")) || bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte("omitted")) {
		t.Fatalf("JSON field contract changed: %s", data)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		t.Fatal(err)
	}
	precisionEqualNumber(t, json.Number(precisionYAMLAt(t, &node, "exact_id").Value), "9007199254740993")
	precisionEqualNumber(t, json.Number(precisionYAMLAt(t, &node, "custom", "seed").Value), "340282366920938463463374607431768211457")
	if precisionYAMLAt(t, &node, "null_value").Tag != "!!null" || precisionYAMLAt(t, &node, "enabled").Tag != "!!bool" {
		t.Fatal("non-numeric scalar types changed")
	}
	for _, value := range precisionYAMLAt(t, &node, "strings").Content {
		if value.Tag != "!!str" {
			t.Fatalf("string was coerced to %s", value.Tag)
		}
	}
	for _, invalid := range []any{json.Number("1 2"), json.Number("01"), make(chan int)} {
		for _, format := range []string{"json", "yaml"} {
			h := managementTestContext("output", format)
			managementTestCode(t, writeOutput(h.ctx.IO, h.ctx.Flags, invalid), "OUTPUT")
			if h.out.Len() != 0 {
				t.Fatal("partially printed invalid output")
			}
		}
	}
}

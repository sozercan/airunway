package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const chatSystemInstruction = "Return only a short answer."

func TestModelChatSystemPromptAndOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.txt")
	if err := os.WriteFile(path, []byte(chatSystemInstruction), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags Flags
		input string
	}{
		{"inline", Flags{"system": {chatSystemInstruction}, "message": {"hello"}}, ""},
		{"file", Flags{"system-file": {path}, "message-file": {"-"}}, "hello"},
		{"stdin", Flags{"system-file": {"-"}, "message": {"hello"}}, chatSystemInstruction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, service := accessTestModel(), accessTestService("actual-api", 8000)
			client := &accessFakeClient{resources: []Object{model, service, accessTestPod(service, nil)}}
			requests := make(chan Object, 1)
			server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				var body Object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "OK"}})})
			})
			accessTestSetupForward(t, client, server, "test")
			tc.flags["temperature"], tc.flags["max-tokens"] = []string{"0.2"}, []string{"32"}
			c, _, _ := accessTestContext(client, tc.flags)
			c.IO.In = strings.NewReader(tc.input)
			accessTestCode(t, runAccess("model", "chat", "llama", c), "")
			body := <-requests
			messages := objects(body["messages"])
			if len(messages) != 2 || stringAt(messages[0], "role") != "system" || stringAt(messages[0], "content") != chatSystemInstruction ||
				stringAt(messages[1], "role") != "user" || stringAt(messages[1], "content") != "hello" {
				t.Fatalf("wrong message ordering: %#v", messages)
			}
			if body["temperature"] != 0.2 || intAt(body, "max_tokens") != 32 {
				t.Fatal("chat options lost")
			}
		})
	}
}

func TestSystemPromptPersistsInInteractiveHistory(t *testing.T) {
	model, service := accessTestModel(), accessTestService("actual-api", 8000)
	client := &accessFakeClient{resources: []Object{model, service, accessTestPod(service, nil)}}
	requests := make(chan []Object, 2)
	server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body Object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- objects(body["messages"])
		accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "OK"}})})
	})
	accessTestSetupForward(t, client, server, "test")
	c, _, _ := accessTestContext(client, Flags{"system": {chatSystemInstruction}})
	c.IO.Interactive = true
	c.IO.In = strings.NewReader("first\nsecond\n")
	accessTestCode(t, runAccess("model", "chat", "llama", c), "")
	for _, count := range []int{2, 4} {
		messages := <-requests
		if len(messages) != count || stringAt(messages[0], "role") != "system" || stringAt(messages[0], "content") != chatSystemInstruction {
			t.Fatalf("lost system prompt: %#v", messages)
		}
	}
}

func TestSystemPromptRejectsInvalidInput(t *testing.T) {
	for _, flags := range []Flags{
		{"system": {"  "}},
		{"system": {chatSystemInstruction}, "system-file": {"not-a-file"}},
		{"system-file": {"not-a-file"}},
		{"system-file": {"-"}, "message-file": {"-"}},
		{"system-file": {"-"}},
	} {
		c, _, _ := accessTestContext(&accessFakeClient{}, flags)
		_, err := accessSystemMessages("model", c)
		accessTestCode(t, err, "USAGE")
		if strings.Contains(err.Error(), chatSystemInstruction) {
			t.Fatal("error leaked prompt")
		}
	}
	c, _, _ := accessTestContext(&accessFakeClient{}, Flags{"system": {chatSystemInstruction}})
	_, err := accessSystemMessages("agent", c)
	accessTestCode(t, err, "USAGE")
}

func TestSystemPromptFileSizeLimit(t *testing.T) {
	c, _, _ := accessTestContext(&accessFakeClient{}, Flags{"system-file": {"-"}, "message": {"hello"}})
	c.IO.In = strings.NewReader(strings.Repeat("x", maxInput+1))
	_, err := accessSystemMessages("model", c)
	accessTestCode(t, err, "USAGE")
}

func TestSystemPromptFlagsAreParsed(t *testing.T) {
	_, flags, err := parseArgs([]string{"model", "chat", "demo", "--system-file", "instructions.txt", "--temperature", "0.5", "--max-tokens", "100"})
	if err != nil || flags.Text("system-file") != "instructions.txt" {
		t.Fatalf("flags: %v %v", flags, err)
	}
}

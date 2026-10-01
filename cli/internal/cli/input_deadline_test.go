package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type heldCLIInput struct {
	entered, release, finished chan struct{}
	reader                     *strings.Reader
	start, close, finish       sync.Once
}

func newHeldCLIInput(t *testing.T, value string) *heldCLIInput {
	t.Helper()
	r := &heldCLIInput{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}), reader: strings.NewReader(value)}
	t.Cleanup(r.unblock)
	return r
}

func (r *heldCLIInput) Read(p []byte) (int, error) {
	r.start.Do(func() { close(r.entered) })
	<-r.release
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.finish.Do(func() { close(r.finished) })
	}
	return n, err
}

func (r *heldCLIInput) unblock() { r.close.Do(func() { close(r.release) }) }

func assertCLIInputBounded(t *testing.T, input *heldCLIInput, cancel context.CancelFunc, work func() error) {
	t.Helper()
	returned := make(chan error, 1)
	go func() { returned <- work() }()
	select {
	case <-input.entered:
	case err := <-returned:
		t.Fatalf("command returned before reading input: %v", err)
	case <-time.After(time.Second):
		t.Fatal("command never read input")
	}
	code, exit := "TIMEOUT", 4
	if cancel != nil {
		cancel()
		code, exit = "CANCELED", 130
	}
	select {
	case err := <-returned:
		agentRunTestError(t, err, code, exit)
	case <-time.After(time.Second):
		input.unblock()
		select {
		case <-returned:
		case <-time.After(time.Second):
		}
		t.Fatal("stalled input ignored timeout or cancellation")
	}
	input.unblock()
	select {
	case <-input.finished:
	case <-time.After(time.Second):
		t.Fatal("released input reader did not finish")
	}
}

func TestModelChatStalledSystemInputIsBounded(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancellation"}[canceled], func(t *testing.T) {
			model, service := accessTestModel(), accessTestService("actual-api", 8000)
			client := &accessFakeClient{resources: []Object{model, service, accessTestPod(service, nil)}}
			c, out, _ := accessTestContext(client, Flags{"system-file": {"-"}, "message": {"hello"}, "timeout": {"80ms"}})
			input := newHeldCLIInput(t, "Be brief.")
			c.IO.In = input
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			c.Context = ctx
			var cancel context.CancelFunc
			if canceled {
				cancel = stop
				c.Flags["timeout"] = []string{"5s"}
			}
			assertCLIInputBounded(t, input, cancel, func() error { return runAccess("model", "chat", "llama", c) })
			if out.Len() != 0 {
				t.Fatal("canceled input produced output")
			}
		})
	}
}

func TestAgentRunStalledInputsDoNotSubmit(t *testing.T) {
	for _, flag := range []string{"task-file", "prompt-file", "config-file"} {
		for _, canceled := range []bool{false, true} {
			t.Run(flag+map[bool]string{false: "/timeout", true: "/cancellation"}[canceled], func(t *testing.T) {
				checkAgentRunStalledInput(t, flag, canceled)
			})
		}
	}
}

func checkAgentRunStalledInput(t *testing.T, flag string, canceled bool) {
	t.Helper()
	client, c, out, _ := agentRunTestSetup(t)
	value := "Be brief."
	if flag == "task-file" {
		delete(c.Flags, "task")
	}
	if flag == "config-file" {
		value = `{"systemPrompt":"Be brief."}`
	}
	input := newHeldCLIInput(t, value)
	c.IO.In, c.Flags[flag], c.Flags["timeout"] = input, []string{"-"}, []string{"80ms"}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	c.Context = ctx
	var cancel context.CancelFunc
	if canceled {
		cancel = stop
		c.Flags["timeout"] = []string{"5s"}
	}
	writes := make(chan struct{}, 1)
	client.onCreate = func(context.Context, Object, bool) (Object, error) {
		writes <- struct{}{}
		return nil, cliError(1, "UNEXPECTED", "late submission")
	}
	assertCLIInputBounded(t, input, cancel, func() error { return runAgentOnce([]string{"agent", "run", "helper"}, c, &CLIConfig{}, false) })
	select {
	case <-writes:
		t.Fatal("submitted a resource after input timeout/cancellation")
	default:
	}
	if out.Len() != 0 {
		t.Fatal("canceled input produced output")
	}
}

package cli

import "strings"

const actionChat = "chat"

func accessSystemMessages(noun string, c *CommandContext) ([]any, error) {
	if !c.Flags.Has("system") && !c.Flags.Has("system-file") {
		return []any{}, nil
	}
	if noun != "model" {
		return nil, usage("System prompts are only supported by model chat. Set agent instructions with --prompt instead.")
	}
	if c.Flags.Text("system-file") == "-" &&
		(c.Flags.Text("message-file") == "-" || (!c.Flags.Has("message") && !c.Flags.Has("message-file"))) {
		return nil, usage("Standard input can supply only one input. Use a file for the system prompt or provide --message.")
	}
	text, _, err := readFlagInput(c.Flags, "system", "system-file", c.IO)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, usage("The system prompt must not be empty.")
	}
	return []any{Object{"role": "system", "content": text}}, nil
}

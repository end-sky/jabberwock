package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type clipboardCommand struct {
	name string
	args []string
}

// clipboardCommands returns common Linux desktop clipboard helpers first and
// then platform-native commands. No clipboard library or Go module is needed.
func clipboardCommands() []clipboardCommand {
	commands := []clipboardCommand{
		{name: "wl-copy"},
		{name: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--input"}},
	}
	switch runtime.GOOS {
	case "darwin":
		commands = append([]clipboardCommand{{name: "pbcopy"}}, commands...)
	case "windows":
		commands = append([]clipboardCommand{{name: "clip.exe"}}, commands...)
	}
	return commands
}

func copyTextToClipboard(value string) (string, error) {
	if value == "" {
		return "", errors.New("text is empty")
	}
	var attempts []string
	for _, candidate := range clipboardCommands() {
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, path, candidate.args...)
		cmd.Stdin = strings.NewReader(value)
		cmd.Stdout = io.Discard
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err = cmd.Run()
		cancel()
		if err == nil {
			return candidate.name, nil
		}
		attempts = append(attempts, candidate.name+": "+err.Error())
	}
	// OSC 52 is supported by many terminal emulators and remote terminal
	// multiplexers. It copies the complete payload (not just visible lines).
	if isTerminal(os.Stdout) {
		encoded := base64.StdEncoding.EncodeToString([]byte(value))
		if _, err := fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\a", encoded); err == nil {
			return "terminal OSC 52", nil
		}
	}
	if len(attempts) == 0 {
		return "", errors.New("no clipboard helper found (install wl-copy, xclip, or xsel; terminal OSC 52 was unavailable)")
	}
	return "", fmt.Errorf("clipboard helpers failed: %s", strings.Join(attempts, "; "))
}

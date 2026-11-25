package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func skillsCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "skills", args...)
	if cmd.Err != nil {
		return cmd
	}

	switch strings.ToLower(filepath.Ext(cmd.Path)) {
	case ".cmd", ".bat":
	default:
		return cmd
	}

	values := append([]string{cmd.Path}, args...)

	var command strings.Builder

	for _, value := range values {
		if strings.ContainsAny(value, "\"\r\n\x00") {
			cmd.Err = fmt.Errorf("cannot safely pass %q to the skills batch file", value)
			return cmd
		}

		if command.Len() != 0 {
			command.WriteByte(' ')
		}

		command.WriteByte('"')
		// cmd.exe expands percent-delimited variables even inside quotes.
		command.WriteString(strings.ReplaceAll(value, "%", "%SKILLS_MISSING_PERCENT%"))
		command.WriteString(strings.Repeat("\\", len(value)-len(strings.TrimRight(value, "\\"))))
		command.WriteByte('"')
	}

	shell := exec.CommandContext(ctx, "cmd.exe")
	shell.Env = append(shell.Environ(), "SKILLS_MISSING_PERCENT=%")
	shell.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: syscall.EscapeArg(shell.Path) + ` /d /s /v:off /c "` + command.String() + `"`,
	}

	return shell
}

//go:build !windows

package main

import (
	"context"
	"os/exec"
)

func skillsCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "skills", args...)
}

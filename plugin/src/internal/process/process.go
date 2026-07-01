package process

import (
	"os"
	"os/exec"
	"strings"
)

// HasExplicitEnv reports whether env contains an assignment for key.
func HasExplicitEnv(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

// PrepareCommand configures cmd so it owns a process group when supported.
func PrepareCommand(cmd *exec.Cmd) {
	prepareCommand(cmd)
}

// CommandStartsProcessGroup is primarily a test helper for platform-specific
// command attributes.
func CommandStartsProcessGroup(cmd *exec.Cmd) bool {
	return commandStartsProcessGroup(cmd)
}

// KillTree terminates process and, when supported, every process in its group.
func KillTree(process *os.Process) error {
	return killTree(process)
}

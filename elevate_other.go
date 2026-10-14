//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func isAdmin() bool { return os.Geteuid() == 0 }

// runElevated re-runs this exe with args through sudo, in this terminal, and returns its exit code.
func runElevated(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command("sudo", append([]string{exe}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

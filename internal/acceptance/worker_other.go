//go:build !windows

package acceptance

import "os/exec"

func hideWorker(_ *exec.Cmd) {}

//go:build !windows

package stream

import "os/exec"

func hideFFmpegWindow(cmd *exec.Cmd) {}

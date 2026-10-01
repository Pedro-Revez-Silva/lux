package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// shellCall is a shell command the agent runs as its shell tool: reported
// started (Done false), then done with its output, as the protocol reports
// a real agent's shell tool (Claude Code's Bash tool_use and tool_result,
// Codex's commandExecution item, OpenCode's execute tool_call).
type shellCall struct {
	ID, Command string
	Done        bool
	Output      string
	ExitCode    int
	// Cancelled: the turn was cancelled while it ran; it was killed.
	Cancelled bool
}

// runShell runs a `sh <command>` line with sh -c in the working directory,
// in a process group of its own that a cancel kills; true if cancelled.
func (a *agent) runShell(command string, cancel chan struct{}) bool {
	call := shellCall{ID: fmt.Sprintf("exec-%d", time.Now().UnixNano()), Command: command}
	a.shell(call)
	c := exec.Command("sh", "-c", command)
	c.Dir = a.cwd
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		done <- err
	} else {
		go func() { done <- c.Wait() }()
	}
	var err error
	select {
	case err = <-done:
	case <-cancel:
		if c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		err = <-done
		call.Cancelled = true
	}
	call.Done, call.Output = true, out.String()
	if err != nil {
		call.ExitCode = 1
		if ee, ok := err.(*exec.ExitError); ok {
			call.ExitCode = ee.ExitCode()
		}
	}
	a.shell(call)
	if call.Cancelled {
		a.say("cancelled")
	}
	return call.Cancelled
}

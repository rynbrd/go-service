package service

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestService(t *testing.T) {
	svc, err := NewService([]string{"sleep", "2"})
	if err != nil {
		t.Errorf("NewService => error{%s}, wanted Service", err)
		return
	}

	pid := svc.Pid()
	if svc.Pid() != 0 {
		t.Errorf("svc.Pid() => %d wanted 0", pid)
	}

	commands := make(chan Command)
	responses := make(chan Response)
	events := make(chan Event)

	processRunning := func() bool {
		pid := svc.Pid()
		if pid == 0 {
			return false
		}
		file := fmt.Sprintf("/proc/%d/stat", pid)
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return false
		}
		return true
	}

	verifyStates := func(states []string) {
		for _, state := range states {
			event := <-events
			if event.Service != svc {
				t.Errorf("event.Service => invalid service pointer")
			}
			if event.State != state {
				t.Errorf("event.State => %s, wanted %s", event.State, state)
			}
			if !processRunning() && event.State == Running {
				t.Errorf("svc.Pid() => not running, should be running")
			} else if processRunning() && (event.State == Stopped || event.State == Exited) {
				t.Errorf("svc.Pid() => running, should not be running")
			}
		}
	}

	verifyCommand := func(command string, states []string, success bool) {
		commands <- Command{command, responses}
		verifyStates(states)

		response := <-responses
		if response.Name != command {
			t.Errorf("response.Name => %s, wanted %s", response.Name, command)
		}
		if response.Success() != success {
			t.Errorf("response.Success() => %t, wanted %t, error{%s}", response.Success(), success, response.Error)
		}

		if len(states) > 0 {
			state := states[len(states)-1]
			if response.Error == nil && (state == Exited || state == Backoff) {
				t.Errorf("response.Error => nil, wanted ExitError")
			}
		}
	}

	// Not Running on Run() and Shutdown works without a Start.
	go svc.Run(commands, events)
	verifyCommand(Stop, []string{}, false)
	verifyCommand(Shutdown, []string{}, true)

	// Start works properly and Shutdown works when Running.
	go svc.Run(commands, events)
	verifyCommand(Start, []string{Starting, Running}, true)
	verifyCommand(Start, []string{}, false)
	verifyCommand(Shutdown, []string{Stopping, Stopped}, true)

	// Stop works properly and Shutdown works after Stopped.
	go svc.Run(commands, events)
	verifyCommand(Start, []string{Starting, Running}, true)
	verifyCommand(Stop, []string{Stopping, Stopped}, true)
	verifyCommand(Shutdown, []string{}, true)

	// Receives Exited and restarts.
	go svc.Run(commands, events)
	verifyCommand(Start, []string{Starting, Running}, true)
	verifyStates([]string{Exited, Starting, Running})
	verifyCommand(Shutdown, []string{Stopping, Stopped}, true)

	// Receives Exited and Shutdown works after Exited.
	svc.StopRestart = false
	go svc.Run(commands, events)
	verifyCommand(Start, []string{Starting, Running}, true)
	verifyStates([]string{Exited})
	verifyCommand(Shutdown, []string{}, true)

	// Receives Backoff and Shutdown works after Backoff.
	svc.StartTimeout = 3 * time.Second
	go svc.Run(commands, events)
	verifyCommand(Start, []string{Starting, Backoff, Starting, Backoff, Starting, Backoff, Starting, Fatal}, false)
	verifyCommand(Shutdown, []string{}, true)
	svc.StartTimeout = 1 * time.Second
}

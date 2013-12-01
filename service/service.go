package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

const (
	// Service defaults.
	DefaultStartTimeout = 1 * time.Second
	DefaultStartRetries = 3
	DefaultStopSignal   = syscall.SIGINT
	DefaultStopTimeout  = 5 * time.Second
	DefaultStopRestart  = true

	// Service commands.
	Start    = "start"
	Stop     = "stop"
	Restart  = "restart"
	Shutdown = "shutdown"

	// Service states.
	Starting = "starting"
	Running  = "running"
	Stopping = "stopping"
	Stopped  = "stopped"
	Exited   = "exited"
	Backoff  = "backoff"
)

// Command is sent to a Service to initiate a state change.
type Command struct {
	Name     string
	Response chan<- Response
}

// respond creates and sends a command Response.
func (cmd Command) respond(service *Service, err error) {
	if cmd.Response != nil {
		cmd.Response <- Response{service, cmd.Name, err}
	}
}

// Response contains the result of a Command.
type Response struct {
	Service *Service
	Name    string
	Error   error
}

// Success returns True if the Command was successful.
func (r Response) Success() bool {
	return r.Error == nil
}

// Event is sent by a Service on a state change.
type Event struct {
	Service *Service // The service from which the event originated.
	State   string   // The new state of the service.
	Error   error    // An error indicating why the service is in Exited or Backoff.
}

// ExitError indicated why the service entered an Exited or Backoff state.
type ExitError string

// Error returns the error message of the ExitError.
func (err ExitError) Error() string {
	return string(err)
}

// Service represents a controllable process. Exported fields may be set to configure the service.
type Service struct {
	Directory    string         // The process's working directory. Defaults to the current directory.
	Environment  []string       // The environment of the process. Defaults to nil which indicatesA the current environment.
	StartTimeout time.Duration  // How long the process has to run before it's considered Running.
	StartRetries int            // How many times to restart a process if it fails to start. Defaults to 3.
	StopSignal   syscall.Signal // The signal to send when stopping the process. Defaults to SIGINT.
	StopTimeout  time.Duration  // How long to wait for a process to stop before sending a SIGKILL. Defaults to 5s.
	StopRestart  bool           // Whether or not to restart the process if it exits unexpectedly. Defaults to true.
	Stdout       io.Writer      // Where to send the process's stdout. Defaults to /dev/null.
	Stderr       io.Writer      // Where to send the process's stderr. Defaults to /dev/null.
	args         []string       // The command line of the process to run.
	command      *exec.Cmd      // The os/exec command running the process.
	state        string         // The state of the Service.
}

// New creates a new service with the default configution.
func NewService(args []string) (svc *Service, err error) {
	if cwd, err := os.Getwd(); err == nil {
		svc = &Service{
			cwd,
			nil,
			DefaultStartTimeout,
			DefaultStartRetries,
			DefaultStopSignal,
			DefaultStopTimeout,
			DefaultStopRestart,
			nil,
			nil,
			args,
			nil,
			Stopped,
		}
	}
	return
}

// State gets the current state of the service.
func (s Service) State() string {
	return s.state
}

// Pid gets the PID of the service or 0 if not Running or Stopping.
func (s Service) Pid() int {
	if s.state != Running && s.state != Stopping {
		return 0
	}
	return s.command.Process.Pid
}

func (s Service) makeCommand() *exec.Cmd {
	cmd := exec.Command(s.args[0], s.args[1:]...)
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	cmd.Stdin = nil
	cmd.Env = s.Environment
	cmd.Dir = s.Directory
	return cmd
}

func (s *Service) Run(commands <-chan Command, events chan<- Event) {
	type ProcessState struct {
		State string
		Error error
	}

	var lastCommand *Command
	states := make(chan ProcessState)
	quit := make(chan bool, 2)
	kill := make(chan int, 2)
	retries := 0

	defer func() {
		close(states)
		close(quit)
		close(kill)
	}()

	sendEvent := func(state string, err error) {
		s.state = state
		events <- Event{s, state, err}
	}

	sendInvalidCmd := func(cmd *Command, state string) {
		if cmd != nil {
			cmd.respond(s, errors.New(fmt.Sprintf("invalid state transition: %s -> %s", s.state, state)))
		}
	}

	start := func(cmd *Command) {
		if s.state != Stopped && s.state != Exited && s.state != Backoff {
			sendInvalidCmd(cmd, Starting)
			return
		}

		sendEvent(Starting, nil)
		go func() {
			s.command = s.makeCommand()
			startTime := time.Now()
			if err := s.command.Start(); err == nil {
				states <- ProcessState{Running, nil}
				exitErr := s.command.Wait()

				msg := ""
				if time.Now().Sub(startTime) < s.StartTimeout {
					if exitErr == nil {
						msg = "process exited prematurely with success"
					} else {
						msg = fmt.Sprintf("process exited prematurely with failure: %s", exitErr)
					}
					states <- ProcessState{Backoff, ExitError(msg)}
				} else {
					if exitErr == nil {
						msg = "process exited normally with success"
					} else {
						msg = fmt.Sprintf("process exited normally with failure: %s", exitErr)
					}
					states <- ProcessState{Exited, ExitError(msg)}
				}
			} else {
				states <- ProcessState{Exited, err}
			}
		}()
	}

	stop := func(cmd *Command) {
		if s.state != Running {
			sendInvalidCmd(cmd, Stopping)
			return
		}

		sendEvent(Stopping, nil)
		pid := s.Pid()
		s.command.Process.Signal(s.StopSignal) //TODO: Check for error.
		go func() {
			time.Sleep(s.StopTimeout)
			defer func() {
				if err := recover(); err != nil {
					if _, ok := err.(runtime.Error); !ok {
						panic(err)
					}
				}
			}()
			kill <- pid
		}()
	}

	shutdown := func(cmd *Command, lastCmd *Command) {
		if lastCmd != nil {
			lastCmd.respond(s, errors.New("service is shutting down"))
		}
		if s.state == Stopped || s.state == Exited {
			quit <- true
		} else if s.state == Running {
			stop(cmd)
		}
	}

	onRunning := func(cmd *Command) {
		sendEvent(Running, nil)
		if cmd != nil {
			switch cmd.Name {
			case Start:
				fallthrough
			case Restart:
				cmd.respond(s, nil)
			case Shutdown:
				stop(cmd)
			}
		}
	}

	onStopped := func(cmd *Command) {
		sendEvent(Stopped, nil)
		if cmd != nil {
			switch cmd.Name {
			case Restart:
				start(cmd)
			case Stop:
				cmd.respond(s, nil)
			case Shutdown:
				quit <- true
			}
		}
	}

	onExited := func(cmd *Command, err error) {
		sendEvent(Exited, err)
		if s.StopRestart {
			start(cmd)
		}
	}

	onBackoff := func(cmd *Command, err error) {
		if retries < s.StartRetries {
			sendEvent(Backoff, err)
			start(cmd)
			retries++
		} else {
			sendEvent(Exited, err)
			retries = 0
		}
	}

loop:
	for {
		select {
		case state := <-states:
			// running, exited
			switch state.State {
			case Running:
				onRunning(lastCommand)
			case Exited:
				if s.state == Stopping {
					onStopped(lastCommand)
				} else {
					onExited(lastCommand, state.Error)
				}
			case Backoff:
				onBackoff(lastCommand, state.Error)
			}
			if lastCommand != nil {
				if lastCommand.Name == Restart && s.state == Running {
					lastCommand = nil
				} else if lastCommand.Name != Restart && lastCommand.Name != Shutdown {
					lastCommand = nil
				}
			}
		case command := <-commands:
			if lastCommand == nil || lastCommand.Name != Shutdown {
				switch command.Name {
				case Start:
					start(&command)
				case Stop:
					stop(&command)
				case Restart:
					stop(&command)
				case Shutdown:
					shutdown(&command, lastCommand)
				}
				lastCommand = &command
			} else {
				command.respond(s, errors.New("service is shutting down"))
			}
		case <-quit:
			if lastCommand != nil {
				lastCommand.respond(s, nil)
			}
			break loop
		case pid := <-kill:
			if pid == s.Pid() {
				s.command.Process.Kill() //TODO: Check for error.
			}
		}
	}
}

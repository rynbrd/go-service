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

	var command *Command = nil
	states := make(chan ProcessState)
	kill := make(chan int, 2)
	retries := 0

	defer func() {
		close(states)
		close(kill)
	}()

	sendResponse := func(err error) {
		if command != nil {
			if command.Response != nil {
				command.respond(s, err)
			}
			command = nil
		}
	}

	sendEvent := func(state string, err error) {
		s.state = state
		events <- Event{s, state, err}

		if command == nil {
			return
		}

		switch command.Name {
		case Restart:
			fallthrough
		case Start:
			if state == Running {
				sendResponse(nil)
			} else if state == Exited {
				sendResponse(err)
			}
		case Stop:
			if state == Stopped {
				sendResponse(nil)
			} else if state == Exited {
				sendResponse(err)
			}
		}
	}

	invalidStateError := func(state string) error {
		return errors.New(fmt.Sprintf("invalid state transition: %s -> %s", s.state, state))
	}

	start := func() {
		if s.state != Stopped && s.state != Exited && s.state != Backoff {
			sendResponse(invalidStateError(Starting))
			return
		}

		sendEvent(Starting, nil)
		go func() {
			s.command = s.makeCommand()
			if err := s.command.Start(); err == nil {
				waitOver := make(chan bool, 1)
				checkOver := make(chan bool, 1)

				defer func() {
					close(waitOver)
					close(checkOver)
				}()

				go func() {
					time.Sleep(s.StartTimeout)
					select {
					case <-waitOver:
						checkOver <-false
					default:
						states <- ProcessState{Running, nil}
						checkOver <-true
					}
				}()

				exitErr := s.command.Wait()
				waitOver <-true

				msg := ""
				if check := <-checkOver; check {
					if exitErr == nil {
						msg = "process exited normally with success"
					} else {
						msg = fmt.Sprintf("process exited normally with failure: %s", exitErr)
					}
					states <- ProcessState{Exited, ExitError(msg)}
				} else {
					if exitErr == nil {
						msg = "process exited prematurely with success"
					} else {
						msg = fmt.Sprintf("process exited prematurely with failure: %s", exitErr)
					}
					states <- ProcessState{Backoff, ExitError(msg)}
				}
			} else {
				states <- ProcessState{Exited, err}
			}
		}()
	}

	stop := func() {
		if s.state != Running {
			sendResponse(invalidStateError(Stopping))
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

	shouldShutdown := func() bool {
		return command != nil && command.Name == Shutdown
	}

	shouldQuit := func() bool {
		return shouldShutdown() && (s.state == Stopped || s.state == Exited)
	}

	for !shouldQuit() {
		select {
		case state := <-states:
			switch state.State {
			case Running:
				if shouldShutdown() {
					stop()
				} else {
					sendEvent(Running, nil)
				}
			case Exited:
				if s.state == Stopping {
					sendEvent(Stopped, nil)
				} else {
					sendEvent(Exited, state.Error)
					if s.StopRestart {
						start()
					}
				}
			case Backoff:
				if s.state == Stopping {
					sendEvent(Stopped, nil)
				} else {
					if retries < s.StartRetries {
						sendEvent(Backoff, state.Error)
						start()
						retries++
					} else {
						sendEvent(Exited, state.Error)
						retries = 0
					}
				}
			}
		case newCommand := <-commands:
			if command != nil {
				if newCommand.Name == Shutdown {
					// Fail previous command to force shutdown.
					command.respond(s, errors.New("service is shuttind down"))
				} else {
					// Don't allow execution of more than one command at a time.
					newCommand.respond(s, errors.New("command %s is currently executing"))
					continue
				}
			}

			command = &newCommand
			switch command.Name {
			case Start:
				start()
			case Stop:
				stop()
			case Restart:
				switch s.state {
				case Running:
					stop()
				case Stopped:
					start()
				case Exited:
					start()
				default:
					sendResponse(invalidStateError(Stopping))
				}
			case Shutdown:
				switch s.state {
				case Running:
					stop()
				case Backoff:
					s.state = Exited
				}
			}
		case pid := <-kill:
			if pid == s.Pid() {
				s.command.Process.Kill() //TODO: Check for error.
			}
		}
	}

	if command != nil {
		command.respond(s, nil)
	}
}

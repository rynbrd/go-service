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
	DefaultStopSignal  = syscall.SIGINT
	DefaultStopTimeout = 5 * time.Second
	DefaultRestart     = true
	DefaultRetries     = 3

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
	//TODO: Implement Backoff state.
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
	Service *Service
	State   string
}

// Service represents a controllable process. Exported fields may be set to configure the service.
type Service struct {
	Directory   string         // The process's working directory. Defaults to the current directory.
	Environment []string       // The environment of the process. Defaults to nil which indicatesA the current environment.
	StopSignal  syscall.Signal // The signal to send when stopping the process. Defaults to SIGINT.
	StopTimeout time.Duration  // How long to wait for a process to stop before sending a SIGKILL. Defaults to 5s.
	Restart     bool           // Whether or not to restart the process if it exits unexpectedly. Defaults to true.
	Retries     int            // How many times to restart a process if it fails to start. Defaults to 3.
	Stdout      io.Writer      // Where to send the process's stdout. Defaults to /dev/null.
	Stderr      io.Writer      // Where to send the process's stderr. Defaults to /dev/null.
	args        []string       // The command line of the process to run.
	command     *exec.Cmd      // The os/exec command running the process.
	state       string         // The state of the Service.
}

// New creates a new service with the default configution.
func NewService(args []string) (svc *Service, err error) {
	if cwd, err := os.Getwd(); err == nil {
		svc = &Service{
			cwd,
			nil,
			DefaultStopSignal,
			DefaultStopTimeout,
			DefaultRestart,
			DefaultRetries,
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

func (s *Service) startProcess(states chan string) (err error) {
	defer func() {
		if err != nil {
			//TODO: Do something with this error.
			states <- Exited
		}
	}()

	s.command = s.makeCommand()
	if err = s.command.Start(); err != nil {
		return
	}

	states <- Running
	s.command.Wait()
	states <- Exited
	return
}

func (s *Service) Run(commands <-chan Command, events chan<- Event) {
	var lastCommand *Command
	states := make(chan string)
	quit := make(chan bool, 2)
	kill := make(chan int, 2)
	retries := 0

	defer func() {
		close(states)
		close(quit)
		close(kill)
	}()

	sendEvent := func(state string) {
		s.state = state
		events <- Event{s, state}
	}

	sendInvalidCmd := func(cmd *Command, state string) {
		if cmd != nil {
			cmd.respond(s, errors.New(fmt.Sprintf("invalid state transition: %s -> %s", s.state, state)))
		}
	}

	start := func(cmd *Command) {
		if s.state != Stopped && s.state != Exited {
			sendInvalidCmd(cmd, Starting)
			return
		}

		sendEvent(Starting)
		go s.startProcess(states)
	}

	stop := func(cmd *Command) {
		if s.state != Running {
			sendInvalidCmd(cmd, Stopping)
			return
		}

		sendEvent(Stopping)
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
		sendEvent(Running)
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
		retries = 0
	}

	onStopped := func(cmd *Command) {
		sendEvent(Stopped)
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

	onExited := func(cmd *Command, retries int) bool {
		sendEvent(Exited)
		if s.Restart && retries < s.Retries {
			start(cmd)
			return true
		}
		return false
	}

loop:
	for {
		select {
		case state := <-states:
			// running, exited
			switch state {
			case Running:
				onRunning(lastCommand)
			case Exited:
				if s.state == Stopping {
					onStopped(lastCommand)
				} else {
					if onExited(lastCommand, retries) {
						retries++
					}
				}
			}
			if lastCommand != nil {
				if lastCommand.Name == Restart && s.state == Running {
					lastCommand = nil
				} else if lastCommand.Name != Restart && lastCommand.Name != Shutdown {
					lastCommand = nil
				}
			}
		case command := <-commands:
			if lastCommand == nil || lastCommand.Name != Shutdown { // Shutdown cannot be overriden!
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

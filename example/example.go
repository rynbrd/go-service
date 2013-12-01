package main

import (
	"../service"
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	commands := make(chan service.Command)
	responses := make(chan service.Response)
	events := make(chan service.Event)
	svc, err := service.NewService([]string{"sleep", "10"})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	go svc.Run(commands, events)
	go func() {
		commands <- service.Command{service.Start, responses}
		time.Sleep(5 * time.Second)
		commands <- service.Command{service.Restart, responses}
		time.Sleep(5 * time.Second)
		commands <- service.Command{service.Stop, responses}
		time.Sleep(5 * time.Second)
		commands <- service.Command{service.Start, responses}
		time.Sleep(15 * time.Second)
		commands <- service.Command{service.Shutdown, responses}
	}()

loop:
	for {
		select {
		case event := <-events:
			fmt.Printf("service is %s\n", event.State)
		case response := <-responses:
			if response.Success() {
				fmt.Printf("command %s succeeded\n", response.Name)
				if response.Name == service.Shutdown {
					break loop
				}
			} else {
				fmt.Printf("command %s failed: %s\n", response.Name, response.Error)
			}
		default:
			runtime.Gosched()
		}
	}
}

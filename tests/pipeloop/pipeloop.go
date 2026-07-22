package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"go.podman.io/buildah/pkg/cli"
)

var pipeloopCmd = &cobra.Command{
	Use:  "pipeloop 'CMD' [...]",
	Long: "A pipeline that loops around to the start",
	RunE: func(_ *cobra.Command, args []string) error {
		n := len(args)
		commands := make([]*exec.Cmd, 0, n)
		pipes := make([][2]*os.File, n)
		results := make([]error, n)
		var err error
		for i := range n {
			pipes[i][0], pipes[i][1], err = os.Pipe()
			if err != nil {
				return err
			}
		}
		for i := range n {
			cmd := exec.Command("sh", "-c", args[i])
			cmd.Stdin = pipes[(i-1+n)%n][0]
			cmd.Stdout = pipes[i][1]
			cmd.Stderr = os.Stderr
			commands = append(commands, cmd)
		}
		var cmds sync.WaitGroup
		cleanup := make(chan int, len(commands))
		for i := range commands {
			if err := commands[i].Start(); err != nil {
				results[i] = fmt.Errorf("%s: %w", args[i], err)
			}
		}
		for i := range pipes {
			if err := pipes[i][0].Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if err := pipes[i][1].Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
		cmds.Go(func() {
			if firstToExit, ok := <-cleanup; ok {
				for i := range commands {
					if i != firstToExit {
						if err := commands[i].Process.Signal(syscall.SIGTERM); err != nil {
							fmt.Fprintln(os.Stderr, err)
						}
					}
				}
			}
		})
		for i := range commands {
			cmds.Go(func() {
				if err := commands[i].Wait(); err != nil {
					results[i] = fmt.Errorf("%s: %w", args[i], err)
				}
				cleanup <- i
			})
		}
		cmds.Wait()
		return errors.Join(results...)
	},
	Args:          cobra.MinimumNArgs(1),
	SilenceErrors: true,
}

func main() {
	var exitCode int
	if err := pipeloopCmd.Execute(); err != nil {
		exitCode = cli.ExecErrorCodeGeneric
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if w, ok := ee.Sys().(syscall.WaitStatus); ok {
				exitCode = w.ExitStatus()
			}
		}
	}
	os.Exit(exitCode)
}

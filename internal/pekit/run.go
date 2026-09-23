package pekit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

func Main(args []string, stdout, stderr io.Writer) int {
	app := &App{Stdout: stdout, Stderr: stderr, Now: time.Now}
	if err := app.Run(args); err != nil {
		var rendered renderedError
		if !errors.As(err, &rendered) {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}

type renderedError struct {
	err error
}

func (e renderedError) Error() string { return e.err.Error() }
func (e renderedError) Unwrap() error { return e.err }

func (a *App) Run(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return wrapDiag("cwd", "get current directory", err)
	}
	if len(args) == 0 {
		// A bare `pekit` is a request for orientation, not a run: show the
		// overview, but on stderr and with a failing status, since nothing
		// was done.
		fmt.Fprint(a.Stderr, usageText(""))
		return renderedError{err: diag("missing_command", "missing command")}
	}
	inv, err := ParseInvocation(args, cwd)
	if err != nil {
		return err
	}
	if inv.Help {
		fmt.Fprint(a.Stdout, usageText(inv.HelpTopic))
		return nil
	}
	if inv.Command == CommandVersion {
		fmt.Fprintln(a.Stdout, versionString())
		return nil
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	renderer := newRenderer(inv, a.Stdout, a.Stderr, a.Now)
	ctx := &Context{App: a, Inv: inv, Renderer: renderer, Start: a.Now(), PublishRegistry: NewDestinationRegistry()}
	for _, suppressed := range inv.Suppressed {
		renderer.Event(Event{Type: "unused_suppressed", Message: suppressed})
	}
	if inv.WorkspaceMode {
		err = runWorkspace(ctx)
	} else {
		err = runRecipe(ctx, "", "")
	}
	if inv.JSON {
		if inv.DryRun {
			if err != nil {
				renderer.Error(err)
			}
			if jr, ok := renderer.(*jsonRenderer); ok {
				jr.FlushPlan(inv)
			}
			if err != nil {
				return renderedError{err: err}
			}
		} else if err != nil {
			renderer.Error(err)
			return renderedError{err: err}
		}
	}
	return err
}

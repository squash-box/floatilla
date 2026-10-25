package main

import (
	"flag"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/squash-box/floatilla/src/internal/env"
	"github.com/squash-box/floatilla/src/internal/tui"
)

func (c *cli) tui(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	demo := fs.Bool("demo", false, "run against a local demo fleet instead of the control plane")
	theme := fs.String("theme", env.String("FLOATILLA_THEME", ""), "dark or light (default: follow the terminal)")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}

	var src tui.Source = tui.NewConnectSource(env.String("FLOATILLA_URL", "http://localhost:7420"))
	if *demo {
		src = tui.NewDemoSource()
	}
	opts := tui.Options{Source: src, Level: tui.DetectColorLevel(nil), Settings: tui.LoadSettings()}
	switch *theme {
	case "":
	case "dark":
		opts.Palette = &tui.Dark
	case "light":
		opts.Palette = &tui.Light
	default:
		return fmt.Errorf("tui: unknown theme %q", *theme)
	}

	app := tui.New(opts)
	defer app.Quit()
	_, err := tea.NewProgram(app, tea.WithContext(c.ctx), tea.WithFPS(10)).Run()
	return err
}

// Command herodotos runs an emergent worldbuilding simulation: regional
// historian agents propose, the simulation engine commits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dnym/herodotos/internal/config"
	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/sim"
	"github.com/dnym/herodotos/internal/store"
	"github.com/dnym/herodotos/internal/web"
)

const usage = `herodotos — emergent world history simulator

Usage:
  herodotos init    --world <world.json> [--db <path>]   create a world database
  herodotos step    [--db <path>]                        advance exactly one step
  herodotos run     --steps N [--db <path>]              advance N consecutive steps
  herodotos inspect [--db <path>] [--events N]           show world state summary
  herodotos serve   [--db <path>] [--addr host:port]     serve the web overview

Inference for step/run/serve comes from the environment:
  HERODOTOS_INFERENCE_BASE_URL   OpenAI-compatible endpoint (…/v1)
  HERODOTOS_INFERENCE_MODEL      model name
  HERODOTOS_INFERENCE_TIMEOUT    Go duration, default 120s

For deterministic offline runs, add --scripted <fixture.json>:
a JSON array of {"role": "...", "text": "..."} replies consumed in order.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "init":
		err = runInit(ctx, args)
	case "step":
		err = runStep(ctx, args)
	case "run":
		err = runRun(ctx, args)
	case "inspect":
		err = runInspect(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		if errors.Is(err, store.ErrLocked) {
			fmt.Fprintln(os.Stderr, "error:", err, "(another herodotos process holds this database)")
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openEngine opens the database and builds the engine from the already
// parsed inference flags; both fail together, with a clear message.
func openEngine(dbPath, scripted *string) (*store.DB, *sim.Engine, error) {
	db, err := store.Open(*dbPath)
	if err != nil {
		return nil, nil, err
	}
	var client inference.Client
	modelName := ""
	if *scripted != "" {
		fake, err := inference.NewScriptedFromFile(*scripted)
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		client, modelName = fake, "scripted"
	} else if inf, ok := config.InferenceFromEnv(); ok {
		client = &inference.HTTPClient{BaseURL: inf.BaseURL, Model: inf.Model, Timeout: inf.Timeout}
		modelName = inf.Model
	} else {
		db.Close()
		return nil, nil, errors.New("no inference configured: set HERODOTOS_INFERENCE_BASE_URL and HERODOTOS_INFERENCE_MODEL, or pass --scripted")
	}
	return db, &sim.Engine{DB: db, Client: client, ModelName: modelName, Budgets: config.DefaultBudgets()}, nil
}

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dbPath := fs.String("db", "world.db", "path to the world database")
	worldPath := fs.String("world", "", "world definition JSON (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *worldPath == "" {
		return errors.New("--world is required")
	}
	spec, err := store.LoadWorldSpec(*worldPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(*dbPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing database %s", *dbPath)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.InitWorld(ctx, spec); err != nil {
		return err
	}
	fmt.Printf("initialized world %q at %s (%d regions, step = %d days)\n",
		spec.Name, *dbPath, len(spec.Regions), spec.StepDays)
	return nil
}

func runStep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("step", flag.ExitOnError)
	dbPath := fs.String("db", "world.db", "path to the world database")
	scripted := fs.String("scripted", "", "replay model replies from a fixture file instead of a live endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, eng, err := openEngine(dbPath, scripted)
	if err != nil {
		return err
	}
	defer db.Close()
	return runSteps(ctx, db, eng, 1)
}

func runRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dbPath := fs.String("db", "world.db", "path to the world database")
	steps := fs.Int("steps", 1, "number of steps to advance")
	scripted := fs.String("scripted", "", "replay model replies from a fixture file instead of a live endpoint")
	// `herodotos run 6 --db x`: a leading bare count reads more naturally
	// than --steps 6 before the other flags, and Go's flag package stops
	// parsing at the first positional argument, so normalize it here.
	if len(args) > 0 && isAllDigits(args[0]) {
		args = append([]string{fmt.Sprintf("-steps=%s", args[0])}, args[1:]...)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	db, eng, err := openEngine(dbPath, scripted)
	if err != nil {
		return err
	}
	defer db.Close()
	return runSteps(ctx, db, eng, *steps)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func runSteps(ctx context.Context, db *store.DB, eng *sim.Engine, steps int) error {
	failures := 0
	for i := 0; i < steps; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := eng.RunStep(ctx)
		if err != nil {
			failures++
			fmt.Fprintln(os.Stderr, "step failed:", err)
			var f *sim.Failure
			if errors.As(err, &f) {
				fmt.Fprintln(os.Stderr, "  diagnostics kept in attempt", f.AttemptID)
			}
			continue
		}
		fmt.Printf("step %d committed: %d event(s), %d model call(s), %d proposal(s)\n",
			res.Step, len(res.EventIDs), res.ModelCalls, res.Proposals)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d steps failed; the world only advanced on committed steps", failures, steps)
	}
	return nil
}

func runInspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dbPath := fs.String("db", "world.db", "path to the world database")
	events := fs.Int("events", 10, "number of recent events to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	meta, err := db.GetMeta(ctx)
	if err != nil {
		return err
	}
	dateLabel := fmt.Sprintf("step %d", meta.ClockStep)
	if t, err := meta.StepDate(meta.ClockStep); err == nil {
		dateLabel = fmt.Sprintf("step %d (%s)", meta.ClockStep, t.Format("2 Jan 2006"))
	}
	fmt.Printf("world %q — %s, %d-day steps\n\n", meta.WorldName, dateLabel, meta.StepDays)

	fmt.Println("REGIONS")
	regions, err := db.ListRegions(ctx)
	if err != nil {
		return err
	}
	for _, r := range regions {
		fmt.Printf("  %-14s %-30s [%s]\n", r.ID, r.Name, r.Status)
	}

	directives, err := db.ListDirectives(ctx)
	if err != nil {
		return err
	}
	if len(directives) > 0 {
		fmt.Println("\nDIRECTIVES")
		for _, d := range directives {
			hard := "soft"
			if d.Hard {
				hard = "hard"
			}
			fmt.Printf("  %s [%s/%s] %s\n", d.ID, hard, d.Status, d.Text)
		}
	}

	fmt.Println("\nRECENT EVENTS")
	evs, err := db.ListEvents(ctx, store.EventQuery{Limit: *events})
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		fmt.Println("  (nothing has happened yet)")
	}
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		fmt.Printf("  %s step %d [%s] %s\n", ev.ID, ev.Step, ev.Type, ev.Title)
	}

	attempts, err := db.ListAttempts(ctx, 5)
	if err != nil {
		return err
	}
	if len(attempts) > 0 {
		fmt.Println("\nRECENT ATTEMPTS")
		for _, a := range attempts {
			line := fmt.Sprintf("  %s step %d [%s]", a.ID, a.Step, a.Status)
			if a.Error != "" {
				line += ": " + a.Error
			}
			fmt.Println(line)
		}
	}
	return nil
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "world.db", "path to the world database")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	scripted := fs.String("scripted", "", "replay model replies from a fixture file instead of a live endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var eng *sim.Engine
	if *scripted != "" {
		fake, err := inference.NewScriptedFromFile(*scripted)
		if err != nil {
			return err
		}
		eng = &sim.Engine{DB: db, Client: fake, ModelName: "scripted", Budgets: config.DefaultBudgets()}
	} else if inf, ok := config.InferenceFromEnv(); ok {
		eng = &sim.Engine{DB: db,
			Client:    &inference.HTTPClient{BaseURL: inf.BaseURL, Model: inf.Model, Timeout: inf.Timeout},
			ModelName: inf.Model, Budgets: config.DefaultBudgets()}
	}

	srv := &web.Server{DB: db, Engine: eng}
	fmt.Printf("herodotos serving %s on http://%s (ctrl-c to stop)\n", db.Path, *addr)
	return srv.ListenAndServe(ctx, *addr)
}

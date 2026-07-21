package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/x2x3studio/hgctl/internal/pipeline"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "dreamctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: dreamctl <bootstrap|prepare|finalize|apply>")
	}
	switch arguments[0] {
	case "bootstrap":
		return runBootstrap(ctx, arguments[1:], stdout, stderr)
	case "prepare":
		return runPrepare(ctx, arguments[1:], stdout, stderr)
	case "finalize":
		return runFinalize(arguments[1:], stdout, stderr)
	case "apply":
		return runApply(ctx, arguments[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runBootstrap(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checkout := flags.String("checkout", "", "clean trusted control checkout")
	controlSHA := flags.String("control-sha", os.Getenv("GITHUB_SHA"), "trusted control commit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bootstrap accepts flags only")
	}
	result, err := pipeline.Bootstrap(ctx, pipeline.BootstrapOptions{Checkout: *checkout, ControlSHA: *controlSHA})
	if err != nil {
		return err
	}
	if err := writeOutputs(map[string]string{"created": strconv.FormatBool(result.Created)}); err != nil {
		return err
	}
	if result.Created {
		_, err = fmt.Fprintln(stdout, "Generated initial shared product")
	} else {
		_, err = fmt.Fprintln(stdout, "Shared branch already exists; nothing to do")
	}
	return err
}

func runPrepare(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "shared checkout with queue refs")
	prompt := flags.String("prompt", "", "trusted Dream prompt")
	model := flags.String("model", "", "fresh model artifact destination")
	control := flags.String("control", "", "fresh control artifact destination")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "owner/repository binding")
	controlSHA := flags.String("control-sha", os.Getenv("GITHUB_SHA"), "trusted control commit")
	runID := flags.String("run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run id")
	runAttempt := flags.Int("run-attempt", envInteger("GITHUB_RUN_ATTEMPT"), "producer run attempt")
	runSlot := flags.Uint64("run-slot", envUnsigned("HOURGLASS_RUN_SLOT"), "queue rotation slot")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("prepare accepts flags only")
	}
	result, err := pipeline.Prepare(ctx, pipeline.PrepareOptions{
		SourceDirectory: *source, PromptPath: *prompt, ModelDirectory: *model,
		ControlDirectory: *control, Repository: *repository, ControlSHA: *controlSHA,
		RunID: *runID, RunAttempt: *runAttempt, RunSlot: *runSlot,
	})
	if err != nil {
		return err
	}
	for _, notice := range result.Notices {
		message := fmt.Sprintf("queue %s %s at %s: %s", notice.Machine, notice.Kind, notice.Commit, notice.Reason)
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			_, _ = fmt.Fprintln(stderr, "::warning::hourglass: "+message)
		} else {
			_, _ = fmt.Fprintln(stderr, "hourglass: "+message)
		}
	}
	outputs := map[string]string{
		"has_work":              strconv.FormatBool(result.HasWork),
		"has_semantic_evidence": strconv.FormatBool(result.HasSemanticEvidence),
		"producer_attempt":      strconv.Itoa(*runAttempt),
		"tool_artifact":         artifactName("tool", *runID, *runAttempt),
		"model_artifact":        artifactName("model", *runID, *runAttempt),
		"control_artifact":      artifactName("control", *runID, *runAttempt),
		"model_path":            *model,
		"control_path":          *control,
	}
	if executable, executableErr := os.Executable(); executableErr == nil {
		outputs["tool_path"] = filepath.Dir(executable)
	} else {
		return executableErr
	}
	if err := writeOutputs(outputs); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Prepared %d event(s); work=%t\n", len(result.Manifest.Events), result.HasWork)
	return err
}

func runFinalize(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("finalize", flag.ContinueOnError)
	flags.SetOutput(stderr)
	model := flags.String("model", "", "model artifact root")
	control := flags.String("control", "", "control artifact root")
	publication := flags.String("publication", "", "fresh sanitized publication destination")
	runID := flags.String("run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run id")
	runAttempt := flags.Int("run-attempt", envInteger("GITHUB_RUN_ATTEMPT"), "finalizer run attempt")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "owner/repository binding")
	controlSHA := flags.String("control-sha", os.Getenv("GITHUB_SHA"), "trusted control commit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("finalize accepts flags only")
	}
	manifest, err := pipeline.Finalize(*model, *control, *publication)
	if err != nil {
		return err
	}
	if manifest.RunID != *runID || manifest.Repository != *repository || manifest.ControlSHA != *controlSHA {
		_ = os.RemoveAll(*publication)
		return errors.New("control manifest belongs to another workflow run")
	}
	outputs := map[string]string{
		"publication_artifact": artifactName("publication", *runID, *runAttempt),
		"publication_path":     *publication,
	}
	if err := writeOutputs(outputs); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Finalized %d publication file(s)\n", len(manifest.Files))
	return err
}

func runApply(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	publication := flags.String("publication", "", "sanitized publication artifact")
	control := flags.String("control", "", "trusted typed control artifact")
	checkout := flags.String("checkout", "", "clean shared publisher checkout")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "owner/repository binding")
	controlSHA := flags.String("control-sha", os.Getenv("GITHUB_SHA"), "trusted control commit")
	runID := flags.String("run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run id")
	producerAttempt := flags.Int("producer-attempt", 0, "prepare job run attempt")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("apply accepts flags only")
	}
	result, err := pipeline.Apply(ctx, pipeline.ApplyOptions{
		Publication: *publication,
		Control:     *control,
		Repository:  *checkout,
		Binding: pipeline.RunBinding{
			Repository: *repository, ControlSHA: *controlSHA,
			RunID: *runID, RunAttempt: *producerAttempt,
		},
	})
	if err != nil {
		return err
	}
	if err := writeOutputs(map[string]string{
		"file_pattern": result.FilePattern,
		"has_changes":  strconv.FormatBool(result.HasChanges),
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Applied %d publication file(s)\n", len(result.Paths))
	return err
}

func artifactName(kind, runID string, attempt int) string {
	return fmt.Sprintf("hourglass-%s-%s-%d", kind, runID, attempt)
}

func writeOutputs(values map[string]string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(value, "\r\n") {
			return errors.New("workflow output contains a line break")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, values[key]); err != nil {
			return err
		}
	}
	return file.Sync()
}

func envInteger(name string) int {
	value, _ := strconv.Atoi(os.Getenv(name))
	return value
}

func envUnsigned(name string) uint64 {
	value, _ := strconv.ParseUint(os.Getenv(name), 10, 64)
	return value
}

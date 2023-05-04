// Package runsummary implements structs that report on a `turbo run` and `turbo run --dry`
package runsummary

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mitchellh/cli"
	"github.com/segmentio/ksuid"
	"github.com/vercel/turbo/cli/internal/client"
	"github.com/vercel/turbo/cli/internal/turbopath"
	"github.com/vercel/turbo/cli/internal/util"
	"github.com/vercel/turbo/cli/internal/workspace"
)

// MissingTaskLabel is printed when a package is missing a definition for a task that is supposed to run
// E.g. if `turbo run build --dry` is run, and package-a doesn't define a `build` script in package.json,
// the RunSummary will print this, instead of the script (e.g. `next build`).
const MissingTaskLabel = "<NONEXISTENT>"

// MissingFrameworkLabel is a string to identify when a workspace doesn't detect a framework
const MissingFrameworkLabel = "<NO FRAMEWORK DETECTED>"

const runSummarySchemaVersion = "0"
const runsEndpoint = "/v0/spaces/%s/runs"
const runsPatchEndpoint = "/v0/spaces/%s/runs/%s"
const tasksEndpoint = "/v0/spaces/%s/runs/%s/tasks"

type runType int

const (
	runTypeReal runType = iota
	runTypeDryText
	runTypeDryJSON
)

// Meta is a wrapper around the serializable RunSummary, with some extra information
// about the Run and references to other things that we need.
type Meta struct {
	RunSummary         *RunSummary
	ui                 cli.Ui
	repoRoot           turbopath.AbsoluteSystemPath // used to write run summary
	repoPath           turbopath.RelativeSystemPath
	singlePackage      bool
	shouldSave         bool
	apiClient          *client.APIClient
	runType            runType
	synthesizedCommand string
	spaceID            string // This is global to the repo
	spaceRun           *spaceRun
}

// RunSummary contains a summary of what happens in the `turbo run` command and why.
type RunSummary struct {
	ID                ksuid.KSUID        `json:"id"`
	Version           string             `json:"version"`
	TurboVersion      string             `json:"turboVersion"`
	GlobalHashSummary *GlobalHashSummary `json:"globalCacheInputs"`
	Packages          []string           `json:"packages"`
	EnvMode           util.EnvMode       `json:"envMode"`
	ExecutionSummary  *executionSummary  `json:"execution,omitempty"`
	Tasks             []*TaskSummary     `json:"tasks"`
	SCM               *scmState          `json:"scm"`
}

// NewRunSummary returns a RunSummary instance
func NewRunSummary(
	startAt time.Time,
	ui cli.Ui,
	repoRoot turbopath.AbsoluteSystemPath,
	repoPath turbopath.RelativeSystemPath,
	turboVersion string,
	apiClient *client.APIClient,
	runOpts util.RunOpts,
	packages []string,
	globalEnvMode util.EnvMode,
	globalHashSummary *GlobalHashSummary,
	synthesizedCommand string,
) Meta {
	singlePackage := runOpts.SinglePackage
	profile := runOpts.Profile
	shouldSave := runOpts.Summarize
	spaceID := runOpts.ExperimentalSpaceID

	runType := runTypeReal
	if runOpts.DryRun {
		runType = runTypeDryText
		if runOpts.DryRunJSON {
			runType = runTypeDryJSON
		}
	}

	executionSummary := newExecutionSummary(synthesizedCommand, repoPath, startAt, profile)

	meta := Meta{
		RunSummary: &RunSummary{
			ID:                ksuid.New(),
			Version:           runSummarySchemaVersion,
			ExecutionSummary:  executionSummary,
			TurboVersion:      turboVersion,
			Packages:          packages,
			EnvMode:           globalEnvMode,
			Tasks:             []*TaskSummary{},
			GlobalHashSummary: globalHashSummary,
			SCM:               getSCMState(repoRoot),
		},
		ui:                 ui,
		runType:            runType,
		repoRoot:           repoRoot,
		singlePackage:      singlePackage,
		shouldSave:         shouldSave,
		apiClient:          apiClient,
		spaceID:            spaceID,
		synthesizedCommand: synthesizedCommand,
	}

	// Bidirectional reference
	meta.spaceRun = &spaceRun{
		apiClient: apiClient,
		ui:        ui,
	}
	// swallowing the error here, what should we do instead?
	if err := meta.spaceRun.start(); err != nil {
		fmt.Printf("%v", err) // TODO: use a logger
	}

	return meta
}

// getPath returns a path to where the runSummary is written.
// The returned path will always be relative to the dir passsed in.
// We don't do a lot of validation, so `../../` paths are allowed.
func (rsm *Meta) getPath() turbopath.AbsoluteSystemPath {
	filename := fmt.Sprintf("%s.json", rsm.RunSummary.ID)
	return rsm.repoRoot.UntypedJoin(filepath.Join(".turbo", "runs"), filename)
}

func (rsm *Meta) CloseTask(task *TaskSummary) error {
	return rsm.spaceRun.postTask(rsm.spaceID, task)
}

// Close wraps up the RunSummary at the end of a `turbo run`.
func (rsm *Meta) Close(ctx context.Context, exitCode int, workspaceInfos workspace.Catalog) error {
	if rsm.runType == runTypeDryJSON || rsm.runType == runTypeDryText {
		return rsm.closeDryRun(workspaceInfos)
	}

	rsm.RunSummary.ExecutionSummary.exitCode = exitCode
	rsm.RunSummary.ExecutionSummary.endedAt = time.Now()

	summary := rsm.RunSummary
	if err := writeChrometracing(summary.ExecutionSummary.profileFilename, rsm.ui); err != nil {
		rsm.ui.Error(fmt.Sprintf("Error writing tracing data: %v", err))
	}

	// TODO: printing summary to local, writing to disk, and sending to API
	// are all the same thng, we should use a strategy similar to cache save/upload to
	// do this in parallel.

	// Otherwise, attempt to save the summary
	// Warn on the error, but we don't need to throw an error
	if rsm.shouldSave {
		if err := rsm.save(); err != nil {
			rsm.ui.Warn(fmt.Sprintf("Error writing run summary: %v", err))
		}
	}

	rsm.printExecutionSummary()

	// If we don't have a spaceID, we can exit now
	if rsm.spaceID == "" {
		return nil
	}

	// TODO: handle errors
	rsm.spaceRun.done(rsm)

	if rsm.spaceRun.url != "" {
		rsm.ui.Output(fmt.Sprintf("Run: %s", rsm.spaceRun.url))
		rsm.ui.Output("")
	}

	return nil
}

// closeDryRun wraps up the Run Summary at the end of `turbo run --dry`.
// Ideally this should be inlined into Close(), but RunSummary doesn't currently
// have context about whether a run was real or dry.
func (rsm *Meta) closeDryRun(workspaceInfos workspace.Catalog) error {
	// Render the dry run as json
	if rsm.runType == runTypeDryJSON {
		rendered, err := rsm.FormatJSON()
		if err != nil {
			return err
		}

		rsm.ui.Output(string(rendered))
		return nil
	}

	return rsm.FormatAndPrintText(workspaceInfos)
}

// TrackTask makes it possible for the consumer to send information about the execution of a task.
func (summary *RunSummary) TrackTask(taskID string) (func(outcome executionEventName, err error, exitCode *int), *TaskExecutionSummary) {
	return summary.ExecutionSummary.run(taskID)
}

// Save saves the run summary to a file
func (rsm *Meta) save() error {
	json, err := rsm.FormatJSON()
	if err != nil {
		return err
	}

	// summaryPath will always be relative to the dir passsed in.
	// We don't do a lot of validation, so `../../` paths are allowed
	summaryPath := rsm.getPath()

	if err := summaryPath.EnsureDir(); err != nil {
		return err
	}

	return summaryPath.WriteFile(json, 0644)
}

// record sends the summary to the API
func (rsm *Meta) record() error {
	return rsm.spaceRun.done()
}

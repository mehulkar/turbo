package runsummary

import (
	"encoding/json"
	"fmt"

	"github.com/mitchellh/cli"
	"github.com/vercel/turbo/cli/internal/ci"
	"github.com/vercel/turbo/cli/internal/client"
)

type spaceRun struct {
	// rsm       *Meta
	apiClient *client.APIClient
	ui        cli.Ui
	id        string
	url       string
}

func (sr *spaceRun) start(rsm *Meta) error {
	if !sr.apiClient.IsLinked() {
		sr.ui.Warn("Failed to post to space because repo is not linked to a Space. Run `turbo link` first.")
		return nil
	}

	// rsm := sr.rsm
	createRunEndpoint := fmt.Sprintf(runsEndpoint, rsm.spaceID)
	payload := newSpacesRunCreatePayload(rsm)
	data, err := json.Marshal(payload)
	if err == nil {
		return fmt.Errorf("Failed to create payload: %v", err)
	}

	resp, err := sr.apiClient.JSONPost(createRunEndpoint, data)
	if err != nil {
		return fmt.Errorf("POST %s: %w", createRunEndpoint, err)
	}

	// deserialize the response into an anonymous struct, because we want two very simple things from it
	response := &struct {
		ID  string
		URL string
	}{}
	if err := json.Unmarshal(resp, response); err != nil {
		return fmt.Errorf("Error unmarshaling response: %w", err)
	}

	sr.id = response.ID
	sr.url = response.URL

	return nil
}

func (sr *spaceRun) done(rsm *Meta) error {
	if !sr.apiClient.IsLinked() {
		sr.ui.Warn("Failed to post to space because repo is not linked to a Space. Run `turbo link` first.")
		return nil
	}

	if rsm.spaceID == "" {
		return fmt.Errorf("No Run ID found to PATCH")
	}

	data, err := json.Marshal(newSpacesDonePayload(rsm.RunSummary))
	if err != nil {
		return fmt.Errorf("Failed to marshal data for done payload")
	}

	url := fmt.Sprintf(runsPatchEndpoint, rsm.spaceID, sr.id)
	if _, err := rsm.apiClient.JSONPatch(url, data); err != nil {
		return fmt.Errorf("PATCH %s: %w", url, err)
	}

	return nil
}

func (sr *spaceRun) postTask(spaceID string, task *TaskSummary) error {
	if !sr.apiClient.IsLinked() {
		sr.ui.Warn("Failed to post to space because repo is not linked to a Space. Run `turbo link` first.")
		return nil
	}

	payload := newSpacesTaskPayload(task)
	// TODO, figure out how to ensure that start() request was done at this point
	if sr.id == "" {
		return fmt.Errorf("No Run ID found to post tasks")
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Failed to marshal data for task")
	}

	url := fmt.Sprintf(tasksEndpoint, spaceID, sr.id)
	if _, err := sr.apiClient.JSONPost(url, data); err != nil {
		return fmt.Errorf("Failed to send %s summary to space: %w", task.TaskID, err)
	}

	return nil
}

type spacesClientSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type spacesRunPayload struct {
	StartTime      int64               `json:"startTime,omitempty"`      // when the run was started
	EndTime        int64               `json:"endTime,omitempty"`        // when the run ended. we should never submit start and end at the same time.
	Status         string              `json:"status,omitempty"`         // Status is "running" or "completed"
	Type           string              `json:"type,omitempty"`           // hardcoded to "TURBO"
	ExitCode       int                 `json:"exitCode,omitempty"`       // exit code for the full run
	Command        string              `json:"command,omitempty"`        // the thing that kicked off the turbo run
	RepositoryPath string              `json:"repositoryPath,omitempty"` // where the command was invoked from
	Context        string              `json:"context,omitempty"`        // the host on which this Run was executed (e.g. Github Action, Vercel, etc)
	Client         spacesClientSummary `json:"client"`                   // Details about the turbo client
	GitBranch      string              `json:"gitBranch"`
	GitSha         string              `json:"gitSha"`

	// TODO: we need to add these in
	// originationUser string
}

// spacesCacheStatus is the same as TaskCacheSummary so we can convert
// spacesCacheStatus(cacheSummary), but change the json tags, to omit local and remote fields
type spacesCacheStatus struct {
	// omitted fields, but here so we can convert from TaskCacheSummary easily
	Local     bool   `json:"-"`
	Remote    bool   `json:"-"`
	Status    string `json:"status"` // should always be there
	Source    string `json:"source,omitempty"`
	TimeSaved int    `json:"timeSaved"`
}

type spacesTask struct {
	Key          string            `json:"key,omitempty"`
	Name         string            `json:"name,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	Hash         string            `json:"hash,omitempty"`
	StartTime    int64             `json:"startTime,omitempty"`
	EndTime      int64             `json:"endTime,omitempty"`
	Cache        spacesCacheStatus `json:"cache,omitempty"`
	ExitCode     int               `json:"exitCode,omitempty"`
	Dependencies []string          `json:"dependencies,omitempty"`
	Dependents   []string          `json:"dependents,omitempty"`
	Logs         string            `json:"log"`
}

func newSpacesRunCreatePayload(rsm *Meta) *spacesRunPayload {
	startTime := rsm.RunSummary.ExecutionSummary.startedAt.UnixMilli()
	context := "LOCAL"
	if name := ci.Constant(); name != "" {
		context = name
	}

	return &spacesRunPayload{
		StartTime:      startTime,
		Status:         "running",
		Command:        rsm.synthesizedCommand,
		RepositoryPath: rsm.repoPath.ToString(),
		Type:           "TURBO",
		Context:        context,
		GitBranch:      rsm.RunSummary.SCM.Branch,
		GitSha:         rsm.RunSummary.SCM.Sha,
		Client: spacesClientSummary{
			ID:      "turbo",
			Name:    "Turbo",
			Version: rsm.RunSummary.TurboVersion,
		},
	}
}

func newSpacesDonePayload(runsummary *RunSummary) *spacesRunPayload {
	endTime := runsummary.ExecutionSummary.endedAt.UnixMilli()
	return &spacesRunPayload{
		Status:   "completed",
		EndTime:  endTime,
		ExitCode: runsummary.ExecutionSummary.exitCode,
	}
}

func newSpacesTaskPayload(taskSummary *TaskSummary) *spacesTask {
	startTime := taskSummary.Execution.startAt.UnixMilli()
	endTime := taskSummary.Execution.endTime().UnixMilli()

	return &spacesTask{
		Key:          taskSummary.TaskID,
		Name:         taskSummary.Task,
		Workspace:    taskSummary.Package,
		Hash:         taskSummary.Hash,
		StartTime:    startTime,
		EndTime:      endTime,
		Cache:        spacesCacheStatus(taskSummary.CacheSummary), // wrapped so we can remove fields
		ExitCode:     *taskSummary.Execution.exitCode,
		Dependencies: taskSummary.Dependencies,
		Dependents:   taskSummary.Dependents,
		Logs:         string(taskSummary.GetLogs()),
	}
}

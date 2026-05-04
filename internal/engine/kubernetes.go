// Execute commands using Kubernetes.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nalgeon/codapi/internal/config"
	"github.com/nalgeon/codapi/internal/fileio"
	"github.com/nalgeon/codapi/internal/logx"
)

// A Kubernetes engine executes a specific sandbox command
// using Kubernetes pods and `create` or `exec` actions.
type Kubernetes struct {
	cfg *config.Config
	cmd *config.Command
	exe string // kubectl
	tmp string // TMPDIR

	json string
	name string
}

// NewKubernetes creates a new Kubernetes engine for a specific command.
func NewKubernetes(cfg *config.Config, sandbox, command string) Engine {
	cmd := cfg.Commands[sandbox][command]
	return &Kubernetes{cfg, cmd, "kubectl", os.TempDir(), "", ""}
}

// Exec executes the command and returns the output.
func (e *Kubernetes) Exec(req Request) Execution {
	// all steps operate in the same temp directory
	dir, err := fileio.MkdirTemp(0777)
	if err != nil {
		err = NewExecutionError("create temp dir", err)
		return Fail(req.ID, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// if the command entry point file is not defined,
	// there is no need to store request files in the temp directory
	if e.cmd.Entry != "" {
		// write request files to the temp directory
		err = e.writeFiles(dir, req.Files)
		var argErr ArgumentError
		if errors.As(err, &argErr) {
			return Fail(req.ID, err)
		} else if err != nil {
			err = NewExecutionError("write files to temp dir", err)
			return Fail(req.ID, err)
		}
	}

	// initialization step
	if e.cmd.Before != nil {
		out := e.execStep(e.cmd.Before, req, dir, nil)
		if !out.OK {
			return out
		}
	}

	// the first step is required
	first, rest := e.cmd.Steps[0], e.cmd.Steps[1:]
	out := e.execStep(first, req, dir, req.Files)

	// the rest are optional
	if out.OK && len(rest) > 0 {
		// each step operates on the results of the previous one,
		// without using the source files - hence `nil` instead of `files`
		for _, step := range rest {
			out = e.execStep(step, req, dir, nil)
			if !out.OK {
				break
			}
		}
	}

	// cleanup step
	if e.cmd.After != nil {
		afterOut := e.execStep(e.cmd.After, req, dir, nil)
		if out.OK && !afterOut.OK {
			return afterOut
		}
	}

	return out
}

// execStep executes a step using the docker container.
func (e *Kubernetes) execStep(step *config.Step, req Request, dir string, files Files) Execution {
	box, err := e.getBox(step, req)
	if err != nil {
		return Fail(req.ID, err)
	}

	err = e.copyFiles(box, dir)
	if err != nil {
		err = NewExecutionError("copy files to temp dir", err)
		return Fail(req.ID, err)
	}

	stdout, stderr, err := e.exec(box, step, req, dir, files)
	if err != nil {
		return Fail(req.ID, err)
	}

	return Execution{
		ID:     req.ID,
		OK:     true,
		Stdout: stdout,
		Stderr: stderr,
	}
}

// getBox selects an appropriate box for the step (if any).
func (e *Kubernetes) getBox(step *config.Step, req Request) (*config.Box, error) {
	if step.Action != actionRun {
		// steps other than "run" use existing containers
		// and do not spin up new ones
		return nil, nil
	}
	var boxName string
	// If the version is set in the step config, use it.
	if step.Version != "" {
		if step.Version == "latest" {
			boxName = step.Box
		} else {
			boxName = step.Box + ":" + step.Version
		}
	} else if req.Version != "" {
		// If the version is set in the request, use it.
		boxName = step.Box + ":" + req.Version
	} else {
		// otherwise, use the latest version
		boxName = step.Box
	}
	box, found := e.cfg.Boxes[boxName]
	if !found {
		return nil, fmt.Errorf("unknown box %s", boxName)
	}
	return box, nil
}

// copyFiles copies box files to the temporary directory.
func (e *Kubernetes) copyFiles(box *config.Box, dir string) error {
	if box == nil || len(box.Files) == 0 {
		return nil
	}
	for _, pattern := range box.Files {
		err := fileio.CopyFiles(pattern, dir, 0444)
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFiles writes request files to the temporary directory.
func (e *Kubernetes) writeFiles(dir string, files Files) error {
	var err error
	files.Range(func(name, content string) bool {
		if name == "" {
			name = e.cmd.Entry
		}
		var path string
		path, err = fileio.JoinDir(dir, name)
		if err != nil {
			err = NewArgumentError(fmt.Sprintf("files[%s]", name), err)
			return false
		}
		err = fileio.WriteFile(path, content, 0444)
		return err == nil
	})
	return err
}

// exec executes the step in the docker container
// using the files from in the temporary directory.
func (e *Kubernetes) exec(box *config.Box, step *config.Step, req Request, dir string, files Files) (stdout string, stderr string, err error) {
	// limit the stdout/stderr size
	prog := NewProgram(step.Timeout, int64(step.NOutput))
	args, err := e.buildArgs(box, step, req, dir)

	if step.Stdin {
		// pass files to container from stdin
		stdin := filesReader(files)
		stdout, stderr, err = prog.RunStdin(stdin, req.ID, e.exe, args...)
	} else {
		// pass files to container from temp directory
		_, stderr, err = prog.Run(req.ID, e.exe, args...)
		exec.Command(e.exe, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod", e.name).Run()
		if out, err := exec.Command(e.exe, "logs", e.name).Output(); err == nil {
			stdout = string(out)
		} else {
			exit := err.(*exec.ExitError)
			stderr = fmt.Sprintf("%s", exit.Stderr)
		}
		os.Remove(e.json)
	}

	if err == nil {
		// success
		return
	}

	exitErr := new(exec.ExitError)
	if errors.As(err, &exitErr) {
		// the problem (if any) is the code, not the execution
		// so we return the error without wrapping into ExecutionError
		stderr, stdout = stdout+stderr, ""
		if stderr != "" {
			err = fmt.Errorf("%s (%s)", stderr, err)
		}
		return
	}

	// other execution error
	err = NewExecutionError("execute code", err)
	return
}

// buildArgs prepares the arguments for the `kubectl` command.
func (e *Kubernetes) buildArgs(box *config.Box, step *config.Step, req Request, dir string) (args []string, err error) {
	command := expandVars(step.Command, req.ID)

	switch step.Action {
	case actionRun:
		file, err := os.CreateTemp("", "*.json")
		if err != nil {
			return nil, err
		}
		pod := kubectlPodData(box, step, req, dir, command)
		metadata := pod["metadata"].(map[string]string)
		e.name = metadata["name"]
		logx.Debug("pod/%s", e.name)
		data, err := json.Marshal(pod)
		if err != nil {
			return nil, err
		}
		_, err = file.Write(data)
		if err != nil {
			return nil, err
		}
		e.json = file.Name()
		args = []string{"create", "-f", e.json}
	case actionExec:
		args = kubectlExecArgs(step, req)
	case actionStop:
		args = kubectlStopArgs(step, req)
	default:
		// should never happen if the config is valid
		args = []string{"version"}
	}

	if step.Action != actionRun {
		args = append(args, command...)
	}
	logx.Debug("%s %v", e.exe, args)
	if e.json != "" {
		data, err := os.ReadFile(e.json)
		if err == nil {
			logx.Debug("%s", data)
		}
	}
	return args, nil
}

// kubectlPodData prepares the arguments for the `kubectl create` command.
func kubectlPodData(box *config.Box, step *config.Step, req Request, dir string, command []string) map[string]any {
	// replace underscores
	name := strings.ReplaceAll(req.ID, "_", "-")

	container := map[string]any{
		"name": name,
		"resources": map[string]any{
			"limits": map[string]any{
				"cpu":    box.CPU,
				"memory": fmt.Sprintf("%dMi", box.Memory),
			},
		},
		"image":           box.Image,
		"imagePullPolicy": "Never",
		"args":            command,
	}
	containers := []*map[string]any{&container}

	// "%s:/sandbox:ro"
	v := fmt.Sprintf(box.Volume, dir)
	f := strings.Split(v, ":")
	hostPath := f[0]
	mountPath := f[1]
	readOnly := f[2] == "ro"

	volume := map[string]any{
		"name": name,
		"hostPath": map[string]any{
			"path": hostPath,
			"type": "Directory",
		},
	}
	volumes := []*map[string]any{&volume}

	if dir != "" {
		// copy hostPath to mountPath
		pod := fmt.Sprintf("%s-cp", name)
		logx.Debug("pod/%s", pod)
		logx.Debug("cp %s %s [start]", hostPath, mountPath)
		overrides := []map[string]any{
			{"op": "add", "path": "/spec/volumes", "value": []map[string]any{
				{"name": pod, "hostPath": map[string]string{"path": hostPath}},
			}},
			{"op": "add", "path": "/spec/containers/0/volumeMounts", "value": []map[string]any{
				{"name": pod, "mountPath": mountPath},
			}},
		}
		data, _ := json.Marshal(overrides)
		out, err := exec.Command("kubectl", "run", pod, "--image=busybox",
			"--override-type=json", fmt.Sprintf("--overrides=%s", data),
			"sleep", "infinity").CombinedOutput()
		if err != nil {
			logx.Log("%s%v", out, err)
		}
		out, err = exec.Command("kubectl", "wait", "--for=condition=Ready", "pod", pod).CombinedOutput()
		if err != nil {
			logx.Log("%s%v", out, err)
		}
		logx.Debug("cp %s %s [tar]", hostPath, mountPath)
		out, err = exec.Command("kubectl", "cp", fmt.Sprintf("%s/main.sh", hostPath), fmt.Sprintf("%s:%s/", pod, mountPath)).CombinedOutput()
		if err != nil {
			logx.Log("%s%v", out, err)
		}
		logx.Debug("cp %s %s [stop]", hostPath, mountPath)
		out, err = exec.Command("kubectl", "delete", "--now", "--wait", "pod", pod).CombinedOutput()
		if err != nil {
			logx.Log("%s%v", out, err)
		}

		mount := map[string]any{
			"name":      name,
			"mountPath": mountPath,
			"readOnly":  readOnly,
		}
		mounts := []*map[string]any{&mount}

		container["volumeMounts"] = mounts
	}

	spec := map[string]any{
		"containers":    containers,
		"restartPolicy": "Never",
		"volumes":       volumes,
	}
	data := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]string{"name": name},
		"spec":       spec,
	}
	return data
}

// kubectlExecArgs prepares the arguments for the `kubectl exec` command.
func kubectlExecArgs(step *config.Step, req Request) []string {
	// :name means executing in the pod passed in the request
	box := strings.Replace(step.Box, ":name", req.ID, 1)
	return []string{actionExec, box}
}

// kubectlStopArgs prepares the arguments for the `kubectl delete` command.
func kubectlStopArgs(step *config.Step, req Request) []string {
	// :name means executing in the pod passed in the request
	box := strings.Replace(step.Box, ":name", req.ID, 1)
	return []string{actionStop, box}
}

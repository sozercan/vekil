package aikit

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
)

// Accelerator is the GPU path a container engine exposes to model containers.
type Accelerator string

const (
	AccelNone         Accelerator = "none"
	AccelNVIDIA       Accelerator = "nvidia"
	AccelAppleSilicon Accelerator = "applesilicon"
)

// Engine names accepted by --runtime.
const (
	EngineAuto   = "auto"
	EngineDocker = "docker"
	EnginePodman = "podman"
)

// Engine is a detected container engine.
type Engine struct {
	Name   string
	Binary string
	Accel  Accelerator
	// MemoryBytes is the memory available to containers when the engine runs in
	// a VM (macOS). Zero means unknown or host memory.
	MemoryBytes int64
	// Notice explains a fallback the user should know about.
	Notice string
	exec   Executor
}

// DetectOptions configures DetectEngine.
type DetectOptions struct {
	// Preference is auto, docker, or podman.
	Preference string
	GOOS       string
	GOARCH     string
}

type podmanMachine struct {
	Name    string `json:"Name"`
	Running bool   `json:"Running"`
	VMType  string `json:"VMType"`
	Memory  string `json:"Memory"`
	Default bool   `json:"Default"`
}

const libkrunMachineHint = "CONTAINERS_MACHINE_PROVIDER=libkrun podman machine init --now"

// DetectEngine selects a container engine. On Apple Silicon macOS it prefers
// podman with a running libkrun machine, which is the only GPU path for AIKit
// images, and otherwise falls back to docker with a notice explaining why.
func DetectEngine(ctx context.Context, executor Executor, opts DetectOptions) (*Engine, error) {
	if executor == nil {
		executor = OSExecutor{}
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := opts.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	preference := strings.TrimSpace(strings.ToLower(opts.Preference))
	if preference == "" {
		preference = EngineAuto
	}
	switch preference {
	case EngineAuto, EngineDocker, EnginePodman:
	default:
		return nil, fmt.Errorf("unsupported container runtime %q: use auto, docker, or podman", opts.Preference)
	}
	if goos == "darwin" {
		return detectDarwinEngine(ctx, executor, preference, goarch)
	}
	return detectHostEngine(ctx, executor, preference)
}

func detectDarwinEngine(ctx context.Context, executor Executor, preference, goarch string) (*Engine, error) {
	var podmanReason string
	var fallbackPodman *Engine
	if preference != EngineDocker {
		podmanPath, lookErr := executor.LookPath(EnginePodman)
		if lookErr != nil {
			podmanReason = "podman not found"
		} else {
			machine, err := runningPodmanMachine(ctx, executor, podmanPath)
			switch {
			case err != nil:
				podmanReason = err.Error()
			case machine == nil:
				podmanReason = "no podman machine is running (start one with `podman machine start`)"
			case strings.EqualFold(machine.VMType, "libkrun") && goarch == "arm64":
				return &Engine{
					Name:        EnginePodman,
					Binary:      podmanPath,
					Accel:       AccelAppleSilicon,
					MemoryBytes: parseMemory(machine.Memory),
					exec:        executor,
				}, nil
			default:
				podmanReason = fmt.Sprintf("podman machine %q uses %s, which has no GPU access", machine.Name, machine.VMType)
				fallbackPodman = &Engine{
					Name:        EnginePodman,
					Binary:      podmanPath,
					Accel:       AccelNone,
					MemoryBytes: parseMemory(machine.Memory),
					exec:        executor,
				}
			}
		}
		if preference == EnginePodman {
			if fallbackPodman != nil {
				fallbackPodman.Notice = podmanReason + "; running on CPU. GPU acceleration needs a libkrun machine: " + libkrunMachineHint
				return fallbackPodman, nil
			}
			return nil, fmt.Errorf("podman is not usable: %s", podmanReason)
		}
	}

	dockerPath, lookErr := executor.LookPath(EngineDocker)
	if lookErr == nil {
		memory, err := dockerMemory(ctx, executor, dockerPath)
		if err == nil {
			engine := &Engine{Name: EngineDocker, Binary: dockerPath, Accel: AccelNone, MemoryBytes: memory, exec: executor}
			if preference == EngineAuto && goarch == "arm64" {
				engine.Notice = podmanReason + "; using docker without GPU acceleration. For GPU acceleration on Apple Silicon use a podman libkrun machine: " + libkrunMachineHint
			}
			return engine, nil
		}
		if preference == EngineDocker {
			return nil, fmt.Errorf("docker is not usable: %w", err)
		}
		if fallbackPodman == nil {
			return nil, fmt.Errorf("no container engine is available: %s; docker: %v", podmanReason, err)
		}
	} else if preference == EngineDocker {
		return nil, fmt.Errorf("docker not found")
	}
	if fallbackPodman != nil {
		fallbackPodman.Notice = podmanReason + "; running on CPU. GPU acceleration needs a libkrun machine: " + libkrunMachineHint
		return fallbackPodman, nil
	}
	return nil, fmt.Errorf("no container engine is available: %s, and docker is not installed", podmanReason)
}

func runningPodmanMachine(ctx context.Context, executor Executor, podmanPath string) (*podmanMachine, error) {
	out, err := executor.Output(ctx, podmanPath, "machine", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("list podman machines: %w", err)
	}
	var machines []podmanMachine
	if err := json.Unmarshal(out, &machines); err != nil {
		return nil, fmt.Errorf("parse podman machine list: %w", err)
	}
	var selected *podmanMachine
	for i := range machines {
		if !machines[i].Running {
			continue
		}
		if selected == nil || machines[i].Default {
			selected = &machines[i]
		}
	}
	return selected, nil
}

func dockerMemory(ctx context.Context, executor Executor, dockerPath string) (int64, error) {
	out, err := executor.Output(ctx, dockerPath, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0, err
	}
	return parseMemory(strings.TrimSpace(string(out))), nil
}

func detectHostEngine(ctx context.Context, executor Executor, preference string) (*Engine, error) {
	candidates := []string{EngineDocker, EnginePodman}
	if preference != EngineAuto {
		candidates = []string{preference}
	}
	var failures []string
	for _, name := range candidates {
		path, err := executor.LookPath(name)
		if err != nil {
			failures = append(failures, name+" not found")
			continue
		}
		if _, err := executor.Output(ctx, path, "info", "--format", engineInfoFormat(name)); err != nil {
			failures = append(failures, fmt.Sprintf("%s is not usable: %v", name, err))
			continue
		}
		engine := &Engine{Name: name, Binary: path, Accel: AccelNone, exec: executor}
		if nvidiaPresent(ctx, executor) {
			engine.Accel = AccelNVIDIA
		}
		return engine, nil
	}
	return nil, fmt.Errorf("no container engine is available: %s", strings.Join(failures, "; "))
}

func engineInfoFormat(name string) string {
	if name == EnginePodman {
		return "{{.Version.Version}}"
	}
	return "{{.ServerVersion}}"
}

func nvidiaPresent(ctx context.Context, executor Executor) bool {
	path, err := executor.LookPath("nvidia-smi")
	if err != nil {
		return false
	}
	out, err := executor.Output(ctx, path, "-L")
	return err == nil && strings.Contains(string(out), "GPU")
}

func parseMemory(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	// podman machine inspect reports MiB; list reports bytes. Anything this
	// small is MiB.
	if parsed > 0 && parsed < 1<<20 {
		return parsed << 20
	}
	return parsed
}

// GPUArgs returns the engine flags that expose the accelerator to a container.
func (e *Engine) GPUArgs() []string {
	switch e.Accel {
	case AccelNVIDIA:
		if e.Name == EnginePodman {
			return []string{"--device", "nvidia.com/gpu=all"}
		}
		return []string{"--gpus", "all"}
	case AccelAppleSilicon:
		return []string{"--device", "/dev/dri"}
	default:
		return nil
	}
}

// Describe returns a short human-readable engine summary.
func (e *Engine) Describe() string {
	switch e.Accel {
	case AccelAppleSilicon:
		return e.Name + " (libkrun machine, GPU)"
	case AccelNVIDIA:
		return e.Name + " (NVIDIA GPU)"
	default:
		return e.Name + " (CPU)"
	}
}

func (e *Engine) output(ctx context.Context, args ...string) ([]byte, error) {
	return e.exec.Output(ctx, e.Binary, args...)
}

func (e *Engine) imageExists(ctx context.Context, image string) bool {
	_, err := e.output(ctx, "image", "inspect", image)
	return err == nil
}

func (e *Engine) pull(ctx context.Context, image string, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}
	return e.exec.Run(ctx, progress, e.Binary, "pull", image)
}

func (e *Engine) create(ctx context.Context, args ...string) (string, error) {
	out, err := e.output(ctx, append([]string{"create"}, args...)...)
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

func (e *Engine) run(ctx context.Context, args ...string) (string, error) {
	out, err := e.output(ctx, append([]string{"run", "-d"}, args...)...)
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

func (e *Engine) remove(ctx context.Context, id string) error {
	_, err := e.output(ctx, "rm", "-f", id)
	return err
}

// combinedLogs returns stdout and stderr log lines. LocalAI logs to stderr.
func (e *Engine) combinedLogs(ctx context.Context, id string, tail int) string {
	var combined strings.Builder
	_ = e.exec.Run(ctx, &combined, e.Binary, "logs", "--tail", strconv.Itoa(tail), id)
	return combined.String()
}

// copyFileOut streams one file out of a container. The returned reader yields
// the file bytes and its size from the tar header.
func (e *Engine) copyFileOut(ctx context.Context, id, containerPath string) (io.Reader, int64, io.Closer, error) {
	stream, err := e.exec.Stream(ctx, e.Binary, "cp", id+":"+containerPath, "-")
	if err != nil {
		return nil, 0, nil, err
	}
	archive := tar.NewReader(stream)
	for {
		header, err := archive.Next()
		if err != nil {
			_ = stream.Close()
			if errors.Is(err, io.EOF) {
				return nil, 0, nil, fmt.Errorf("%s not found in container", containerPath)
			}
			if failure, ok := stream.(interface{ Err() error }); ok && failure.Err() != nil {
				return nil, 0, nil, failure.Err()
			}
			return nil, 0, nil, fmt.Errorf("read %s from container: %w", containerPath, err)
		}
		switch header.Typeflag {
		case tar.TypeReg:
			return archive, header.Size, stream, nil
		case tar.TypeSymlink:
			_ = stream.Close()
			return nil, 0, nil, fmt.Errorf("%s is a symlink to %s; point the model config at the file itself", containerPath, header.Linkname)
		}
	}
}

// ContainerInfo is the subset of engine inspect output the launcher uses.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func (e *Engine) inspect(ctx context.Context, ids ...string) ([]ContainerInfo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := e.output(ctx, append([]string{"container", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var infos []ContainerInfo
	if err := json.Unmarshal(out, &infos); err != nil {
		return nil, fmt.Errorf("parse %s inspect output: %w", e.Name, err)
	}
	return infos, nil
}

func (e *Engine) listByLabel(ctx context.Context, label string) ([]ContainerInfo, error) {
	out, err := e.output(ctx, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+label)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	return e.inspect(ctx, ids...)
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// hostPort returns the loopback host port published for containerPort.
func (c ContainerInfo) hostPort(containerPort string) (string, bool) {
	for _, binding := range c.NetworkSettings.Ports[containerPort] {
		if strings.TrimSpace(binding.HostPort) != "" {
			return strings.TrimSpace(binding.HostPort), true
		}
	}
	return "", false
}

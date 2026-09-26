package aikit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDetectEngineDarwinPrefersLibkrunPodman(t *testing.T) {
	fake := newFakeEngine(t)
	fake.outputs["podman machine list"] = `[{"Name":"podman-machine-default","Running":true,"VMType":"libkrun","Memory":"23999807488"}]`
	engine, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "darwin", GOARCH: "arm64"})
	if err != nil {
		t.Fatalf("DetectEngine: %v", err)
	}
	if engine.Name != EnginePodman || engine.Accel != AccelAppleSilicon || engine.MemoryBytes != 23999807488 || engine.Notice != "" {
		t.Fatalf("engine = %+v", engine)
	}
	if got := strings.Join(engine.GPUArgs(), " "); got != "--device /dev/dri" {
		t.Fatalf("GPU args = %q", got)
	}
}

func TestDetectEngineDarwinFallsBackToDockerWithReason(t *testing.T) {
	tests := []struct {
		name     string
		machines string
		noPodman bool
		reason   string
	}{
		{name: "podman missing", noPodman: true, reason: "podman not found"},
		{name: "machine stopped", machines: `[{"Name":"m","Running":false,"VMType":"libkrun"}]`, reason: "no podman machine is running"},
		{name: "applehv machine", machines: `[{"Name":"m","Running":true,"VMType":"applehv","Memory":"8589934592"}]`, reason: `podman machine "m" uses applehv`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeEngine(t)
			if tt.noPodman {
				delete(fake.paths, "podman")
			} else {
				fake.outputs["podman machine list"] = tt.machines
			}
			fake.outputs["docker info"] = "8589934592\n"
			engine, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "darwin", GOARCH: "arm64"})
			if err != nil {
				t.Fatalf("DetectEngine: %v", err)
			}
			if engine.Name != EngineDocker || engine.Accel != AccelNone || engine.MemoryBytes != 8589934592 {
				t.Fatalf("engine = %+v", engine)
			}
			if !strings.Contains(engine.Notice, tt.reason) || !strings.Contains(engine.Notice, "without GPU acceleration") {
				t.Fatalf("notice = %q, want reason %q", engine.Notice, tt.reason)
			}
		})
	}
}

func TestDetectEngineDarwinUsesCPUPodmanWithoutDocker(t *testing.T) {
	fake := newFakeEngine(t)
	delete(fake.paths, "docker")
	fake.outputs["podman machine list"] = `[{"Name":"m","Running":true,"VMType":"applehv","Memory":"8589934592"}]`
	engine, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "darwin", GOARCH: "arm64"})
	if err != nil {
		t.Fatalf("DetectEngine: %v", err)
	}
	if engine.Name != EnginePodman || engine.Accel != AccelNone || !strings.Contains(engine.Notice, "running on CPU") {
		t.Fatalf("engine = %+v", engine)
	}
}

func TestDetectEngineDarwinNothingRunning(t *testing.T) {
	fake := newFakeEngine(t)
	fake.outputs["podman machine list"] = `[{"Name":"m","Running":false,"VMType":"libkrun"}]`
	fake.failures["docker info"] = errors.New("Cannot connect to the Docker daemon")
	_, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "darwin", GOARCH: "arm64"})
	if err == nil || !strings.Contains(err.Error(), "podman machine start") || !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("error = %v", err)
	}
}

func TestDetectEngineExplicitPreference(t *testing.T) {
	fake := newFakeEngine(t)
	fake.outputs["podman machine list"] = `[{"Name":"m","Running":true,"VMType":"libkrun","Memory":"8589934592"}]`
	fake.outputs["docker info"] = "1\n"
	engine, err := DetectEngine(context.Background(), fake, DetectOptions{Preference: "docker", GOOS: "darwin", GOARCH: "arm64"})
	if err != nil || engine.Name != EngineDocker || engine.Notice != "" {
		t.Fatalf("docker preference = %+v, %v", engine, err)
	}
	fake.outputs["podman machine list"] = `[]`
	if _, err := DetectEngine(context.Background(), fake, DetectOptions{Preference: "podman", GOOS: "darwin", GOARCH: "arm64"}); err == nil {
		t.Fatal("podman preference without a machine succeeded")
	}
	if _, err := DetectEngine(context.Background(), fake, DetectOptions{Preference: "nerdctl"}); err == nil {
		t.Fatal("unknown runtime accepted")
	}
}

func TestDetectEngineLinuxNVIDIA(t *testing.T) {
	fake := newFakeEngine(t)
	fake.paths["nvidia-smi"] = "/usr/bin/nvidia-smi"
	fake.outputs["docker info"] = "27.0.0\n"
	fake.outputs["nvidia-smi -L"] = "GPU 0: NVIDIA L4 (UUID: GPU-1)\n"
	engine, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatalf("DetectEngine: %v", err)
	}
	if engine.Name != EngineDocker || engine.Accel != AccelNVIDIA || strings.Join(engine.GPUArgs(), " ") != "--gpus all" {
		t.Fatalf("engine = %+v", engine)
	}
	engine.Name = EnginePodman
	if got := strings.Join(engine.GPUArgs(), " "); got != "--device nvidia.com/gpu=all" {
		t.Fatalf("podman GPU args = %q", got)
	}
}

func TestDetectEngineLinuxFallsBackToPodman(t *testing.T) {
	fake := newFakeEngine(t)
	fake.failures["docker info"] = errors.New("permission denied")
	fake.outputs["podman info"] = "5.6.0\n"
	engine, err := DetectEngine(context.Background(), fake, DetectOptions{GOOS: "linux", GOARCH: "amd64"})
	if err != nil || engine.Name != EnginePodman || engine.Accel != AccelNone {
		t.Fatalf("engine = %+v, %v", engine, err)
	}
}

func TestParseMemory(t *testing.T) {
	for input, want := range map[string]int64{"": 0, "junk": 0, "22888": 22888 << 20, "23999807488": 23999807488} {
		if got := parseMemory(input); got != want {
			t.Fatalf("parseMemory(%q) = %d, want %d", input, got, want)
		}
	}
}

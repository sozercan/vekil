package aikit

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeContainer is a container in the fake engine.
type fakeContainer struct {
	id      string
	name    string
	image   string
	labels  map[string]string
	env     map[string]string
	args    []string
	running bool
	exit    int
	oom     bool
	port    string
	logs    string
}

// fakeEngine emulates the docker/podman CLI surface vekil uses.
type fakeEngine struct {
	t *testing.T

	mu         sync.Mutex
	paths      map[string]string
	outputs    map[string]string // "name arg0 arg1..." prefix -> stdout for detection commands
	failures   map[string]error
	images     map[string]map[string][]byte // image -> path -> file contents
	pullable   map[string]bool
	containers map[string]*fakeContainer
	order      []string
	nextID     int
	runs       [][]string
	pulls      []string

	// onRun decides how a started container behaves.
	onRun func(c *fakeContainer)
	// runErrAfterCreate makes run fail after creating its container, like a
	// CLI that loses the daemon before printing the ID.
	runErrAfterCreate error
	// servers backs published ports with real HTTP listeners.
	servers []*httptest.Server
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{
		t:          t,
		paths:      map[string]string{"docker": "/usr/bin/docker", "podman": "/usr/bin/podman"},
		outputs:    map[string]string{},
		failures:   map[string]error{},
		images:     map[string]map[string][]byte{},
		pullable:   map[string]bool{},
		containers: map[string]*fakeContainer{},
	}
	t.Cleanup(func() {
		for _, server := range f.servers {
			server.Close()
		}
	})
	return f
}

// serveLocalAI starts a LocalAI-like HTTP server and returns its port.
func (f *fakeEngine) serveLocalAI(ready func() bool, contextSize func() int) string {
	f.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/readyz":
			if ready == nil || ready() {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasPrefix(r.URL.Path, "/api/models/config-json/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": strings.TrimPrefix(r.URL.Path, "/api/models/config-json/"), "context_size": contextSize()})
		default:
			http.NotFound(w, r)
		}
	}))
	f.servers = append(f.servers, server)
	parsed, _ := url.Parse(server.URL)
	return parsed.Port()
}

func (f *fakeEngine) engine(name string, accel Accelerator) *Engine {
	return &Engine{Name: name, Binary: f.paths[name], Accel: accel, exec: f}
}

func (f *fakeEngine) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path, ok := f.paths[name]; ok {
		return path, nil
	}
	return "", errors.New("not found")
}

func (f *fakeEngine) key(name string, args []string) string {
	base := name
	for engine, path := range f.paths {
		if path == name {
			base = engine
		}
	}
	return strings.TrimSpace(base + " " + strings.Join(args, " "))
}

func (f *fakeEngine) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(name, args)
	for prefix, err := range f.failures {
		if strings.HasPrefix(key, prefix) {
			return nil, err
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(key, prefix) {
			return []byte(out), nil
		}
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %q", key)
	}
	switch args[0] {
	case "image":
		if len(args) == 3 && args[1] == "inspect" {
			if _, ok := f.images[args[2]]; ok {
				return []byte("[{}]"), nil
			}
			return nil, errors.New("no such image")
		}
	case "create":
		image := args[len(args)-1]
		if _, ok := f.images[image]; !ok {
			return nil, fmt.Errorf("no such image %s", image)
		}
		c := f.newContainer(image, args[1:len(args)-1])
		return []byte(c.id + "\n"), nil
	case "run":
		return f.run(args[1:])
	case "rm":
		id := args[len(args)-1]
		for key, c := range f.containers {
			if c.id == id || c.name == id {
				delete(f.containers, key)
			}
		}
		return nil, nil
	case "container":
		if len(args) >= 2 && args[1] == "inspect" {
			return f.inspect(args[2:])
		}
	case "ps":
		return f.ps(args[1:]), nil
	}
	return nil, fmt.Errorf("unexpected command %q", key)
}

func (f *fakeEngine) newContainer(image string, args []string) *fakeContainer {
	f.nextID++
	c := &fakeContainer{
		id:     fmt.Sprintf("c%04d", f.nextID),
		image:  image,
		labels: map[string]string{},
		env:    map[string]string{},
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--label":
			key, value, _ := strings.Cut(args[i+1], "=")
			c.labels[key] = value
			i++
		case "-e":
			key, value, _ := strings.Cut(args[i+1], "=")
			c.env[key] = value
			i++
		case "--name":
			c.name = args[i+1]
			i++
		case "-p", "-v", "--pull", "--gpus", "--device":
			i++
		}
	}
	if c.name == "" {
		c.name = c.id
	}
	f.containers[c.id] = c
	f.order = append(f.order, c.id)
	return c
}

func (f *fakeEngine) run(args []string) ([]byte, error) {
	// args: -d <flags...> image [runner args]
	flags := args[1:]
	imageIndex := -1
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case "--name", "--pull", "--label", "-p", "-e", "-v", "--gpus", "--device":
			i++
			continue
		}
		imageIndex = i
		break
	}
	if imageIndex < 0 {
		return nil, errors.New("run: no image")
	}
	image := flags[imageIndex]
	c := f.newContainer(image, flags[:imageIndex])
	c.args = append([]string(nil), flags[imageIndex+1:]...)
	c.running = true
	f.runs = append(f.runs, append([]string(nil), args...))
	if f.onRun != nil {
		f.mu.Unlock()
		f.onRun(c)
		f.mu.Lock()
	}
	if f.runErrAfterCreate != nil {
		return nil, f.runErrAfterCreate
	}
	return []byte(c.id + "\n"), nil
}

func (f *fakeEngine) inspect(ids []string) ([]byte, error) {
	var infos []map[string]any
	for _, id := range ids {
		c, ok := f.containers[id]
		if !ok {
			return nil, fmt.Errorf("no such container %s", id)
		}
		ports := map[string]any{}
		if c.port != "" {
			ports[containerPort] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": c.port}}
		}
		status := "exited"
		if c.running {
			status = "running"
		}
		infos = append(infos, map[string]any{
			"Id":              c.id,
			"Name":            "/" + c.name,
			"State":           map[string]any{"Status": status, "Running": c.running, "OOMKilled": c.oom, "ExitCode": c.exit},
			"Config":          map[string]any{"Labels": c.labels},
			"NetworkSettings": map[string]any{"Ports": ports},
		})
	}
	return json.Marshal(infos)
}

func (f *fakeEngine) ps(args []string) []byte {
	var filter string
	for i := 0; i < len(args); i++ {
		if args[i] == "--filter" {
			filter = strings.TrimPrefix(args[i+1], "label=")
		}
	}
	key, value, hasValue := strings.Cut(filter, "=")
	var ids []string
	for _, id := range f.order {
		c, ok := f.containers[id]
		if !ok {
			continue
		}
		labelValue, present := c.labels[key]
		if !present || (hasValue && labelValue != value) {
			continue
		}
		ids = append(ids, c.id)
	}
	return []byte(strings.Join(ids, "\n"))
}

func (f *fakeEngine) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) != 3 || args[0] != "cp" || args[2] != "-" {
		return nil, fmt.Errorf("unexpected stream %v", args)
	}
	id, path, _ := strings.Cut(args[1], ":")
	c, ok := f.containers[id]
	if !ok {
		return nil, fmt.Errorf("no such container %s", id)
	}
	data, ok := f.images[c.image][path]
	if !ok {
		return nil, fmt.Errorf("Error: Could not find the file %s in container %s", path, id)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	_ = writer.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path, "/"), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = writer.Write(data)
	_ = writer.Close()
	return io.NopCloser(&archive), nil
}

func (f *fakeEngine) Run(ctx context.Context, w io.Writer, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "pull":
		f.pulls = append(f.pulls, args[1])
		if !f.pullable[args[1]] {
			return fmt.Errorf("manifest unknown: %s", args[1])
		}
		if _, ok := f.images[args[1]]; !ok {
			f.images[args[1]] = map[string][]byte{}
		}
		_, _ = fmt.Fprintf(w, "pulled %s\n", args[1])
		return nil
	case "logs":
		id := args[len(args)-1]
		if c, ok := f.containers[id]; ok {
			_, _ = io.WriteString(w, c.logs)
		}
		return nil
	}
	return fmt.Errorf("unexpected run %v", args)
}

func premadeImageFiles(config string, gguf []byte) map[string][]byte {
	return map[string][]byte{
		"/config.yaml":            []byte(config),
		"/models/model-Q4_K.gguf": gguf,
	}
}

const premadeConfig = `- name: qwen-3.8-27b
  backend: llama-cpp
  parameters:
    model: model-Q4_K.gguf
  template:
    use_tokenizer_template: true
`

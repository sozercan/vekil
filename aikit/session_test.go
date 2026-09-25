package aikit

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startTestSession(t *testing.T, fake *fakeEngine, engine *Engine, opts Options) (*Session, string, error) {
	t.Helper()
	var progress bytes.Buffer
	opts.Engine = engine
	opts.Progress = &progress
	if opts.Environment == nil {
		opts.Environment = []string{}
	}
	if opts.LoadTimeout == 0 {
		opts.LoadTimeout = 5 * time.Second
	}
	session, err := Start(context.Background(), opts)
	return session, progress.String(), err
}

func TestStartPremadeImage(t *testing.T) {
	fake := newFakeEngine(t)
	image := "ghcr.io/kaito-project/aikit/qwen3.8:27b"
	fake.pullable[image] = true
	fake.onRun = func(c *fakeContainer) {
		size, _ := strconv.Atoi(c.env["LOCALAI_CONTEXT_SIZE"])
		c.port = fake.serveLocalAI(nil, func() int { return size })
		c.logs = "load_tensors: offloaded 65/65 layers to GPU\n"
	}

	ref, _ := ParseReference("qwen3.8:27b")
	engine := fake.engine(EngineDocker, AccelNVIDIA)
	engine.exec = &pullSeeder{fakeEngine: fake, files: premadeImageFiles(premadeConfig, testGGUF("qwen35", 262144))}
	session, progress, err := startTestSession(t, fake, engine, Options{Reference: ref, MinimumContextTokens: AgentMinimumContextTokens})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, progress)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	if session.ModelName != "qwen-3.8-27b" || session.ContextTokens != 65536 || session.TrainedContextTokens != 262144 || session.Backend != BackendLlamaCPP {
		t.Fatalf("session = %+v", session)
	}
	if !strings.HasPrefix(session.OpenAIBaseURL(), "http://127.0.0.1:") || !strings.HasSuffix(session.OpenAIBaseURL(), "/v1") {
		t.Fatalf("base URL = %q", session.OpenAIBaseURL())
	}
	if len(fake.pulls) != 1 || fake.pulls[0] != image {
		t.Fatalf("pulls = %v", fake.pulls)
	}
	run := strings.Join(fake.runs[0], " ")
	for _, want := range []string{"--pull never", "-p 127.0.0.1::8080", "--gpus all", "LOCALAI_CONTEXT_SIZE=65536", "LOCALAI_LOAD_TO_MEMORY=qwen-3.8-27b", LabelKeep + "=false", image} {
		if !strings.Contains(run, want) {
			t.Fatalf("run args %q missing %q", run, want)
		}
	}
	if strings.Contains(run, "HF_TOKEN") {
		t.Fatalf("image run passed HF_TOKEN: %q", run)
	}
	for _, c := range fake.containers {
		if c.labels[LabelManaged] == "probe" {
			t.Fatalf("probe container %s was not removed", c.id)
		}
	}

	if !strings.Contains(progress, "qwen-3.8-27b loaded") {
		t.Fatalf("progress = %q", progress)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(fake.containers) != 0 {
		t.Fatalf("containers left after Close: %d", len(fake.containers))
	}
}

func seededPremade(t *testing.T, config string, trained uint32) (*fakeEngine, Reference) {
	t.Helper()
	fake := newFakeEngine(t)
	ref, _ := ParseReference("qwen3.8:27b")
	fake.images[ref.Image] = premadeImageFiles(config, testGGUF("qwen35", trained))
	return fake, ref
}

func TestStartHalvesContextAfterOOM(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.images[strings.Replace(ref.Image, "/aikit/", "/aikit/applesilicon/", 1)] = fake.images[ref.Image]
	attempts := 0
	fake.onRun = func(c *fakeContainer) {
		attempts++
		size, _ := strconv.Atoi(c.env["LOCALAI_CONTEXT_SIZE"])
		if size > AgentMinimumContextTokens {
			c.running = false
			c.exit = 1
			c.logs = "llama_init_from_model: failed to allocate buffer for kv cache\n"
			return
		}
		c.port = fake.serveLocalAI(nil, func() int { return size })
	}
	session, progress, err := startTestSession(t, fake, fake.engine(EnginePodman, AccelAppleSilicon), Options{Reference: ref, MinimumContextTokens: AgentMinimumContextTokens})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, progress)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if attempts != 2 || session.ContextTokens != AgentMinimumContextTokens {
		t.Fatalf("attempts = %d, context = %d", attempts, session.ContextTokens)
	}
	if !strings.Contains(progress, "ran out of memory at context 65536; retrying at 49152") {
		t.Fatalf("progress = %q", progress)
	}
	if !strings.Contains(fake.runs[0][len(fake.runs[0])-1], "applesilicon/qwen3.8:27b") {
		t.Fatalf("image = %v", fake.runs[0])
	}
	if len(fake.containers) != 1 {
		t.Fatalf("failed attempt containers were not removed: %d", len(fake.containers))
	}
}

func TestStartExplicitContextIsNotHalved(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.onRun = func(c *fakeContainer) {
		c.running = false
		c.exit = 137
		c.oom = true
		c.logs = "Killed\n"
	}
	_, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref, ContextTokens: 65536, MinimumContextTokens: AgentMinimumContextTokens})
	if err == nil || !strings.Contains(err.Error(), "ran out of memory loading context 65536") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.runs) != 1 || len(fake.containers) != 0 {
		t.Fatalf("runs = %d, containers left = %d", len(fake.runs), len(fake.containers))
	}
}

func TestStartReportsNonOOMFailure(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.onRun = func(c *fakeContainer) {
		c.running = false
		c.exit = 2
		c.logs = "error: could not load model: unknown architecture\n"
	}
	_, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref})
	if err == nil || !strings.Contains(err.Error(), "exited with code 2") || !strings.Contains(err.Error(), "unknown architecture") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.runs) != 1 {
		t.Fatalf("non-OOM failure was retried %d times", len(fake.runs))
	}
}

func TestStartTimesOutWhileLoading(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(func() bool { return false }, func() int { return 0 })
		c.logs = "loading...\n"
	}
	_, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref, LoadTimeout: 1200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "raise --load-timeout") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.containers) != 0 {
		t.Fatal("timed-out container was not removed")
	}
}

func TestStartRejectsServedContextMismatch(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 8192 })
	}
	_, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref})
	if err == nil || !strings.Contains(err.Error(), "reports context_size 8192") {
		t.Fatalf("error = %v", err)
	}
}

func TestStartUsesPinnedContextWithoutEnv(t *testing.T) {
	config := strings.Replace(premadeConfig, "  backend: llama-cpp\n", "  backend: llama-cpp\n  context_size: 57344\n", 1)
	fake, ref := seededPremade(t, config, 262144)
	fake.onRun = func(c *fakeContainer) {
		if _, set := c.env["LOCALAI_CONTEXT_SIZE"]; set {
			t.Errorf("pinned image received LOCALAI_CONTEXT_SIZE")
		}
		c.port = fake.serveLocalAI(nil, func() int { return 57344 })
	}
	session, progress, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref, MinimumContextTokens: AgentMinimumContextTokens})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if session.ContextTokens != 57344 || !strings.Contains(progress, "fixes context_size 57344") {
		t.Fatalf("context = %d, progress = %q", session.ContextTokens, progress)
	}
}

func TestStartRejectsModelBelowAgentFloor(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 16384)
	_, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref, MinimumContextTokens: AgentMinimumContextTokens})
	if err == nil || !strings.Contains(err.Error(), "trained for 16384") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.runs) != 0 {
		t.Fatal("a container started for an unusable model")
	}
}

func TestStartMemoryPreflight(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	engine := fake.engine(EnginePodman, AccelAppleSilicon)
	engine.MemoryBytes = 100
	_, _, err := startTestSession(t, fake, engine, Options{Reference: ref})
	if err == nil || !strings.Contains(err.Error(), "podman machine set --memory") {
		t.Fatalf("error = %v", err)
	}
}

func TestStartKeepReusesRunningContainer(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	engine := fake.engine(EngineDocker, AccelNone)
	first, _, err := startTestSession(t, fake, engine, Options{Reference: ref, Keep: true})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := first.Close(context.Background()); err != nil || len(fake.containers) != 1 {
		t.Fatalf("kept container removed: %v, containers = %d", err, len(fake.containers))
	}
	if !strings.Contains(first.KeepHint(), "docker rm -f "+first.ContainerName) {
		t.Fatalf("keep hint = %q", first.KeepHint())
	}
	second, progress, err := startTestSession(t, fake, engine, Options{Reference: ref, Keep: true})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !second.Reused || second.ContainerID != first.ContainerID || second.ContextTokens != 65536 || second.ModelName != "qwen-3.8-27b" {
		t.Fatalf("second = %+v", second)
	}
	if len(fake.runs) != 1 || !strings.Contains(progress, "reusing running container") {
		t.Fatalf("runs = %d, progress = %q", len(fake.runs), progress)
	}
}

func TestStartReapsOrphans(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	host := hostName()
	fake.mu.Lock()
	dead := fake.newContainer(ref.Image, []string{"--label", LabelManaged + "=true", "--label", LabelOwner + "=" + host + "/999999999"})
	kept := fake.newContainer(ref.Image, []string{"--label", LabelManaged + "=true", "--label", LabelKeep + "=true", "--label", LabelOwner + "=" + host + "/999999999"})
	alive := fake.newContainer(ref.Image, []string{"--label", LabelManaged + "=true", "--label", LabelOwner + "=" + host + "/" + strconv.Itoa(os.Getppid())})
	elsewhere := fake.newContainer(ref.Image, []string{"--label", LabelManaged + "=true", "--label", LabelOwner + "=other-host/999999999"})
	fake.mu.Unlock()
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	session, _, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{Reference: ref})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if _, ok := fake.containers[dead.id]; ok {
		t.Fatal("orphan from a dead process survived")
	}
	for _, c := range []*fakeContainer{kept, alive, elsewhere} {
		if _, ok := fake.containers[c.id]; !ok {
			t.Fatalf("container %v was reaped", c.labels)
		}
	}
}

func runnerSource(t *testing.T, requireToken bool, sawToken, sawRange *atomic.Bool) *httptest.Server {
	t.Helper()
	gguf := testGGUF("llama", 131072)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawToken.Store(true)
		}
		if requireToken && r.Header.Get("Authorization") != "Bearer hf_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Range") != "" {
			sawRange.Store(true)
		}
		http.ServeContent(w, r, "tiny.gguf", time.Time{}, bytes.NewReader(gguf))
	}))
	t.Cleanup(source.Close)
	return source
}

func TestStartRunnerGGUFFromHuggingFace(t *testing.T) {
	var sawToken, sawRange atomic.Bool
	source := runnerSource(t, true, &sawToken, &sawRange)
	fake := newFakeEngine(t)
	runner := "ghcr.io/kaito-project/aikit/runners/llama-cpp-cpu:latest"
	fake.images[runner] = map[string][]byte{}
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	ref, err := ParseReference("hf.co/org/repo/tiny.gguf")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	client := &http.Client{Transport: rewriteTransport{target: source.URL, base: http.DefaultTransport}}
	session, progress, err := startTestSession(t, fake, fake.engine(EnginePodman, AccelAppleSilicon), Options{
		Reference:   ref,
		HTTPClient:  client,
		Environment: []string{"HF_TOKEN=hf_test"},
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, progress)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if session.ModelName != "tiny" || session.TrainedContextTokens != 131072 || !sawRange.Load() {
		t.Fatalf("session = %+v, range = %v", session, sawRange.Load())
	}
	run := strings.Join(fake.runs[0], " ")
	for _, want := range []string{"-e HF_TOKEN", "-v vekil-aikit-", ":/models", runner + " " + ref.Source, "LOCALAI_LOAD_TO_MEMORY=tiny"} {
		if !strings.Contains(run, want) {
			t.Fatalf("run args %q missing %q", run, want)
		}
	}
	if strings.Contains(run, "hf_test") || strings.Contains(run, "/dev/dri") {
		t.Fatalf("run args leak the token or pass a GPU device to the CPU runner: %q", run)
	}
	if !strings.Contains(progress, "no Apple Silicon runner images") || !strings.Contains(progress, "on podman (CPU runner)") {
		t.Fatalf("progress = %q", progress)
	}
}

func TestStartRunnerGGUFElsewhereGetsNoHuggingFaceToken(t *testing.T) {
	var sawToken, sawRange atomic.Bool
	source := runnerSource(t, false, &sawToken, &sawRange)
	fake := newFakeEngine(t)
	fake.images["ghcr.io/kaito-project/aikit/runners/llama-cpp-cpu:latest"] = map[string][]byte{}
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	ref, err := ParseReference("https://models.example.com/models/tiny.gguf")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	session, progress, err := startTestSession(t, fake, fake.engine(EngineDocker, AccelNone), Options{
		Reference:   ref,
		HTTPClient:  &http.Client{Transport: rewriteTransport{target: source.URL, base: http.DefaultTransport}},
		Environment: []string{"HF_TOKEN=hf_test"},
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, progress)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if sawToken.Load() {
		t.Fatal("HF_TOKEN was sent to a non-Hugging Face host")
	}
	if run := strings.Join(fake.runs[0], " "); strings.Contains(run, "HF_TOKEN") {
		t.Fatalf("runner received HF_TOKEN for a non-Hugging Face source: %q", run)
	}
}

func TestStartRunnerRepositoryExplainsVLLMCPPPool(t *testing.T) {
	config := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"max_position_embeddings": 40960}`))
	}))
	defer config.Close()
	fake := newFakeEngine(t)
	runner := "ghcr.io/kaito-project/aikit/runners/vllm-cpp-cpu:latest"
	fake.images[runner] = map[string][]byte{}
	ref, _ := ParseReference("hf.co/Qwen/Qwen3-0.6B@" + strings.Repeat("a", 40))
	engine := fake.engine(EngineDocker, AccelNone)
	client := config.Client()
	client.Transport = rewriteTransport{target: config.URL, base: http.DefaultTransport}
	_, _, err := startTestSession(t, fake, engine, Options{Reference: ref, HTTPClient: client})
	if err == nil || !strings.Contains(err.Error(), "num_blocks") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.runs) != 0 {
		t.Fatal("started a vllm-cpp runner that cannot hold an agent context")
	}
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

// RoundTrip sends requests for the remote model hosts to the test server and
// everything else, such as container readiness checks, to its real destination.
func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "huggingface.co" && req.URL.Host != "models.example.com" {
		return r.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	target, _ := http.NewRequest(req.Method, r.target+req.URL.Path, nil)
	clone.URL = target.URL
	clone.Host = target.URL.Host
	return r.base.RoundTrip(clone)
}

func TestStartBackendValidation(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	engine := fake.engine(EngineDocker, AccelNone)
	if _, _, err := startTestSession(t, fake, engine, Options{Reference: ref, Backend: "vllm-cpp"}); err == nil || !strings.Contains(err.Error(), "declares its own backend") {
		t.Fatalf("image backend override error = %v", err)
	}
	if _, _, err := startTestSession(t, fake, engine, Options{Reference: ref, Backend: "diffusers"}); err == nil || !strings.Contains(err.Error(), "unsupported backend") {
		t.Fatalf("unknown backend error = %v", err)
	}
	repo, _ := ParseReference("hf.co/Qwen/Qwen3-0.6B@" + strings.Repeat("a", 40))
	if _, _, err := startTestSession(t, fake, engine, Options{Reference: repo, Backend: "llama-cpp"}); err == nil || !strings.Contains(err.Error(), "served by vllm-cpp") {
		t.Fatalf("repo backend error = %v", err)
	}
}

func TestStartApplesiliconFallsBackToStandardImage(t *testing.T) {
	fake := newFakeEngine(t)
	ref, _ := ParseReference("gpt-oss:20b")
	fake.pullable[ref.Image] = true
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	session, progress, err := startTestSessionAfterPull(t, fake, fake.engine(EnginePodman, AccelAppleSilicon), ref)
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, progress)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if session.Image != ref.Image || !strings.Contains(progress, "falling back to "+ref.Image) {
		t.Fatalf("image = %q, progress = %q", session.Image, progress)
	}
	if len(fake.pulls) != 2 || !strings.Contains(fake.pulls[0], "/applesilicon/") {
		t.Fatalf("pulls = %v", fake.pulls)
	}
}

// startTestSessionAfterPull seeds image files when the fake engine pulls them.
func startTestSessionAfterPull(t *testing.T, fake *fakeEngine, engine *Engine, ref Reference) (*Session, string, error) {
	t.Helper()
	files := premadeImageFiles(premadeConfig, testGGUF("gpt-oss", 131072))
	pulling := &pullSeeder{fakeEngine: fake, files: files}
	engine.exec = pulling
	return startTestSession(t, fake, engine, Options{Reference: ref})
}

type pullSeeder struct {
	*fakeEngine
	files map[string][]byte
}

func (p *pullSeeder) Run(ctx context.Context, w io.Writer, name string, args ...string) error {
	err := p.fakeEngine.Run(ctx, w, name, args...)
	if err == nil && args[0] == "pull" {
		p.mu.Lock()
		p.images[args[1]] = p.files
		p.mu.Unlock()
	}
	return err
}

func TestStartReapsOrphanedProbeContainers(t *testing.T) {
	fake, ref := seededPremade(t, premadeConfig, 262144)
	fake.mu.Lock()
	probe := fake.newContainer(ref.Image, []string{"--label", LabelManaged + "=probe", "--label", LabelOwner + "=" + hostName() + "/999999999"})
	fake.mu.Unlock()
	var probeOwner string
	fake.onRun = func(c *fakeContainer) {
		c.port = fake.serveLocalAI(nil, func() int { return 65536 })
	}
	engine := fake.engine(EngineDocker, AccelNone)
	engine.exec = &probeObserver{fakeEngine: fake, owner: &probeOwner}
	session, _, err := startTestSession(t, fake, engine, Options{Reference: ref})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if _, ok := fake.containers[probe.id]; ok {
		t.Fatal("orphaned probe container survived")
	}
	if probeOwner != ownerLabel() {
		t.Fatalf("probe owner label = %q, want %q", probeOwner, ownerLabel())
	}
}

// probeObserver records the owner label of probe containers.
type probeObserver struct {
	*fakeEngine
	owner *string
}

func (p *probeObserver) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "create" {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--label" && strings.HasPrefix(args[i+1], LabelOwner+"=") {
				*p.owner = strings.TrimPrefix(args[i+1], LabelOwner+"=")
			}
		}
	}
	return p.fakeEngine.Output(ctx, name, args...)
}

func TestRemoteInspectionRefusesHTTPSDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request followed a downgrade redirect with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/m.gguf", http.StatusFound)
	}))
	defer secure.Close()
	ref := Reference{Kind: RefRunnerGGUF, Source: secure.URL + "/m.gguf", ModelName: "m"}
	_, err := inspectRemoteGGUF(context.Background(), secure.Client(), ref, "hf_test")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect from https to http") {
		t.Fatalf("error = %v", err)
	}
}

package aikit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Container labels. Every container vekil creates carries LabelManaged so
// leftovers from a crashed process can be found and removed.
const (
	LabelManaged = "dev.vekil.aikit"
	LabelOwner   = "dev.vekil.aikit.owner"
	LabelKeep    = "dev.vekil.aikit.keep"
	LabelSpec    = "dev.vekil.aikit.spec"
	LabelModel   = "dev.vekil.aikit.model"
	LabelContext = "dev.vekil.aikit.context"
	LabelBackend = "dev.vekil.aikit.backend"
)

const (
	// DefaultLoadTimeout bounds pulling nothing but loading the model.
	DefaultLoadTimeout = 10 * time.Minute
	containerPort      = "8080/tcp"
	cleanupTimeout     = 30 * time.Second
	readyPollInterval  = 500 * time.Millisecond
	statePollEvery     = 4
	readyRequestLimit  = 3 * time.Second
	logTailLines       = 200
)

// Options configures Start.
type Options struct {
	Reference Reference
	// Selector picks one model from an image config holding several; it
	// defaults to the reference's #model selector.
	Selector string
	// Backend overrides the reference's default backend for runner references.
	Backend string
	// ContextTokens is an explicit context; zero chooses the default.
	ContextTokens int
	// MinimumContextTokens rejects smaller contexts; zero disables the floor.
	MinimumContextTokens int
	// Keep leaves the container running after Close and reuses a matching
	// running container.
	Keep        bool
	LoadTimeout time.Duration
	Engine      *Engine
	// Progress receives status lines and pull progress.
	Progress    io.Writer
	HTTPClient  *http.Client
	Environment []string
}

// Session is a running AIKit model container.
type Session struct {
	engine *Engine

	ContainerID   string
	ContainerName string
	Image         string
	// BaseURL is the container's loopback LocalAI root, without /v1.
	BaseURL              string
	ModelName            string
	Backend              string
	ContextTokens        int
	TrainedContextTokens int
	Reused               bool
	Keep                 bool

	closeMu sync.Mutex
	closed  bool
}

// OpenAIBaseURL is the provider base URL for the session.
func (s *Session) OpenAIBaseURL() string { return s.BaseURL + "/v1" }

// EngineName is the container engine running the session.
func (s *Session) EngineName() string { return s.engine.Name }

// Close removes the container unless the session keeps it running.
func (s *Session) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed || s.Keep {
		return nil
	}
	// A failed removal can be retried by a later Close.
	if err := s.engine.remove(ctx, s.ContainerID); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// Discard removes a container this process started, even one marked keep,
// when startup is rolled back. A reused container belongs to an earlier run.
func (s *Session) Discard(ctx context.Context) error {
	if s == nil || s.Reused {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.engine.remove(ctx, s.ContainerID); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// KeepHint tells the user how to stop a kept container.
func (s *Session) KeepHint() string {
	return fmt.Sprintf("%s left running as %s; stop it with `%s rm -f %s`", s.ModelName, s.ContainerName, s.engine.Name, s.ContainerName)
}

type statusPrinter struct{ w io.Writer }

func (p statusPrinter) printf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(p.w, "vekil: aikit: "+format+"\n", args...)
}

// Start pulls, inspects, and runs a model container, waits until LocalAI has
// loaded the model, and verifies the served context.
func Start(ctx context.Context, opts Options) (*Session, error) {
	if opts.Engine == nil {
		return nil, fmt.Errorf("aikit: container engine is required")
	}
	engine := opts.Engine
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}
	status := statusPrinter{w: progress}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	loadTimeout := opts.LoadTimeout
	if loadTimeout <= 0 {
		loadTimeout = DefaultLoadTimeout
	}
	environment := opts.Environment
	if environment == nil {
		environment = os.Environ()
	}
	if opts.ContextTokens < 0 {
		return nil, fmt.Errorf("context size must not be negative")
	}

	if err := reapOrphans(ctx, engine); err != nil {
		// An orphan may still hold the memory this model needs.
		return nil, fmt.Errorf("remove orphaned aikit containers: %w", err)
	}

	ref := opts.Reference
	if opts.Selector == "" {
		opts.Selector = ref.Selector
	}
	backend, err := ResolveBackend(ref, opts.Backend)
	if err != nil {
		return nil, err
	}

	image, notice := resolveImage(ref, engine, backend)
	if notice != "" {
		status.printf("%s", notice)
	}
	spec := specHash(image, backend, ref.Source, opts.Selector, opts.ContextTokens, opts.MinimumContextTokens)
	if opts.Keep {
		if session := findReusable(ctx, engine, spec, client); session != nil {
			status.printf("reusing running container %s (%s, context %d)", session.ContainerName, session.ModelName, session.ContextTokens)
			return session, nil
		}
	}

	image, err = ensureImage(ctx, engine, ref, image, status, progress)
	if err != nil {
		return nil, err
	}

	token := ""
	if ref.UsesHuggingFace() {
		token = huggingFaceToken(environment)
	}
	var plan modelPlan
	switch ref.Kind {
	case RefImage:
		plan, err = inspectImage(ctx, engine, image, opts.Selector)
	case RefRunnerGGUF:
		plan, err = inspectRemoteGGUF(ctx, client, ref, token)
	case RefRunnerRepo:
		plan, err = inspectRemoteRepository(ctx, client, ref, token)
	default:
		err = fmt.Errorf("unsupported aikit reference kind")
	}
	if err != nil {
		return nil, err
	}
	plan.Image = image
	if ref.IsRunner() {
		plan.Backend = backend
		plan.RunnerArgs = []string{ref.Source}
		plan.CacheVolume = "vekil-aikit-" + shortHash(backend+"\x00"+ref.Source)
	}

	decision, err := decideContext(plan, opts.ContextTokens, opts.MinimumContextTokens)
	if err != nil {
		return nil, err
	}
	if decision.Warning != "" {
		status.printf("%s", decision.Warning)
	}
	if err := checkMemory(engine, plan); err != nil {
		return nil, err
	}

	// --load-timeout bounds loading across out-of-memory retries, not each one.
	loadDeadline := time.Now().Add(loadTimeout)
	for {
		session, failure, err := runOnce(ctx, engine, plan, decision, opts, spec, token != "", time.Until(loadDeadline), client, status)
		if err != nil {
			return nil, err
		}
		if failure == nil {
			return session, nil
		}
		if failure.oom && decision.Retryable && time.Until(loadDeadline) > 0 {
			next, ok := halvedContext(decision.Tokens, opts.MinimumContextTokens)
			if ok {
				status.printf("%s ran out of memory at context %d; retrying at %d", plan.ModelName, decision.Tokens, next)
				decision.Tokens = next
				decision.Env = llamaCPPContextEnv(next)
				continue
			}
		}
		return nil, failure.err(plan, decision)
	}
}

// ResolveBackend returns the backend that serves ref, checking an explicit
// request against the reference.
func ResolveBackend(ref Reference, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	backend := ref.DefaultBackend()
	if requested != "" {
		normalized, ok := normalizeBackend(requested)
		if !ok {
			return "", fmt.Errorf("unsupported backend %q: use llama-cpp or vllm-cpp", requested)
		}
		if !ref.IsRunner() {
			return "", fmt.Errorf("--backend applies to runner references (hf.co/... or https .gguf URLs); image %s declares its own backend", ref.Redacted())
		}
		if ref.Kind == RefRunnerRepo && normalized != BackendVLLMCPP {
			return "", fmt.Errorf("repository references are served by vllm-cpp; use an https .gguf URL for llama-cpp")
		}
		backend = normalized
	}
	// A vllm.cpp runner cannot be given an agent-sized KV pool; fail here
	// rather than after a pull and download.
	if ref.IsRunner() && backend == BackendVLLMCPP {
		return "", fmt.Errorf("vllm.cpp runner references are not supported: %w", errVLLMCPPPoolSizing)
	}
	return backend, nil
}

func resolveImage(ref Reference, engine *Engine, backend string) (string, string) {
	switch ref.Kind {
	case RefImage:
		if ref.Premade && engine.Accel == AccelAppleSilicon {
			return strings.Replace(ref.Image, PremadeRegistry+"/", PremadeRegistry+"/applesilicon/", 1), ""
		}
		return ref.Image, ""
	default:
		variant := "cpu"
		notice := ""
		switch {
		case engine.Accel == AccelNVIDIA && backend == BackendLlamaCPP:
			variant = "cuda"
		case engine.Accel == AccelNVIDIA:
			notice = "the vllm.cpp CUDA runner targets Blackwell GPUs only; using the CPU runner"
		case engine.Accel == AccelAppleSilicon:
			notice = "AIKit publishes no Apple Silicon runner images; the runner uses the CPU"
		}
		return PremadeRegistry + "/runners/" + backend + "-" + variant + ":latest", notice
	}
}

// ensureImage pulls image when it is missing. A pre-made Apple Silicon image
// that does not exist falls back to the standard image, which runs on the CPU.
func ensureImage(ctx context.Context, engine *Engine, ref Reference, image string, status statusPrinter, progress io.Writer) (string, error) {
	if engine.imageExists(ctx, image) {
		return image, nil
	}
	status.printf("pulling %s", image)
	err := engine.pull(ctx, image, progress)
	if err == nil {
		return image, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if ref.Premade && image != ref.Image {
		status.printf("%s is not available (%v); falling back to %s on the CPU", image, err, ref.Image)
		if engine.imageExists(ctx, ref.Image) {
			return ref.Image, nil
		}
		status.printf("pulling %s", ref.Image)
		if fallbackErr := engine.pull(ctx, ref.Image, progress); fallbackErr != nil {
			return "", fmt.Errorf("pull %s: %w", ref.Image, fallbackErr)
		}
		return ref.Image, nil
	}
	return "", fmt.Errorf("pull %s: %w", image, err)
}

func checkMemory(engine *Engine, plan modelPlan) error {
	if engine.MemoryBytes <= 0 || plan.WeightsBytes <= 0 {
		return nil
	}
	if plan.WeightsBytes <= engine.MemoryBytes*9/10 {
		return nil
	}
	fix := "raise Docker Desktop's memory limit (Settings > Resources)"
	if engine.Name == EnginePodman {
		fix = fmt.Sprintf("podman machine stop && podman machine set --memory %d && podman machine start", (plan.WeightsBytes*2)>>20)
	}
	return fmt.Errorf("%s needs about %.1f GiB for its weights alone, but the %s VM has %.1f GiB; %s",
		plan.ModelName, gib(plan.WeightsBytes), engine.Name, gib(engine.MemoryBytes), fix)
}

func gib(value int64) float64 { return float64(value) / (1 << 30) }

type loadFailure struct {
	exitCode  int
	oomKilled bool
	oom       bool
	timedOut  bool
	logs      string
	cause     error
}

func (f *loadFailure) err(plan modelPlan, decision contextDecision) error {
	tail := lastLines(f.logs, 20)
	switch {
	case f.cause != nil:
		return fmt.Errorf("%s did not load: %w\n%s", plan.ModelName, f.cause, tail)
	case f.timedOut:
		return fmt.Errorf("%s did not finish loading in time (raise --load-timeout)\n%s", plan.ModelName, tail)
	case f.oom:
		return fmt.Errorf("%s ran out of memory loading context %d; pass a smaller --context-size or give the container engine more memory\n%s", plan.ModelName, decision.Tokens, tail)
	default:
		return fmt.Errorf("%s container exited with code %d while loading\n%s", plan.ModelName, f.exitCode, tail)
	}
}

func runOnce(
	ctx context.Context,
	engine *Engine,
	plan modelPlan,
	decision contextDecision,
	opts Options,
	spec string,
	passHFToken bool,
	loadTimeout time.Duration,
	client *http.Client,
	status statusPrinter,
) (_ *Session, failure *loadFailure, err error) {
	name := "vekil-aikit-" + randomSuffix()
	args := []string{
		"--name", name,
		"--pull", "never",
		"--label", LabelManaged + "=true",
		"--label", LabelOwner + "=" + ownerLabel(),
		"--label", LabelKeep + "=" + strconv.FormatBool(opts.Keep),
		"--label", LabelSpec + "=" + spec,
		"--label", LabelModel + "=" + plan.ModelName,
		"--label", LabelContext + "=" + strconv.Itoa(decision.Tokens),
		"--label", LabelBackend + "=" + plan.Backend,
		"-p", "127.0.0.1::8080",
	}
	cpuRunner := plan.CacheVolume != "" && strings.Contains(plan.Image, "-cpu:")
	// A pre-made model whose applesilicon/ variant was unavailable runs the
	// standard image, which cannot use the Apple GPU.
	cpuFallback := opts.Reference.Premade && engine.Accel == AccelAppleSilicon && plan.Image == opts.Reference.Image
	if !cpuRunner && !cpuFallback {
		args = append(args, engine.GPUArgs()...)
	}
	for _, value := range decision.Env {
		args = append(args, "-e", value)
	}
	args = append(args, "-e", "LOCALAI_LOAD_TO_MEMORY="+plan.ModelName)
	if passHFToken && plan.CacheVolume != "" {
		// The engine CLI reads the value from its own environment, which keeps
		// the token off the command line.
		args = append(args, "-e", "HF_TOKEN")
	}
	if plan.CacheVolume != "" {
		args = append(args, "-v", plan.CacheVolume+":/models")
	}
	args = append(args, plan.Image)
	args = append(args, plan.RunnerArgs...)

	where := engine.Describe()
	switch {
	case cpuRunner:
		where = engine.Name + " (CPU runner)"
	case cpuFallback:
		where = engine.Name + " (CPU)"
	}
	status.printf("starting %s on %s (context %d)", plan.ModelName, where, decision.Tokens)
	id, err := engine.run(ctx, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", plan.Image, err)
	}
	session := &Session{
		engine:               engine,
		ContainerID:          id,
		ContainerName:        name,
		Image:                plan.Image,
		ModelName:            plan.ModelName,
		Backend:              plan.Backend,
		ContextTokens:        decision.Tokens,
		TrainedContextTokens: plan.TrainedContext,
		Keep:                 opts.Keep,
	}
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		removeErr := engine.remove(removeCtx, id)
		if removeErr == nil {
			return
		}
		// A container that could not be removed still holds memory, so report
		// it instead of retrying the load beside it.
		removeErr = fmt.Errorf("remove %s: %w", name, removeErr)
		if failure != nil {
			err = errors.Join(failure.err(plan, decision), removeErr)
			failure = nil
			return
		}
		err = errors.Join(err, removeErr)
	}()

	infos, err := engine.inspect(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	if len(infos) == 0 {
		return nil, nil, fmt.Errorf("inspect %s: container not found", name)
	}
	port, ok := infos[0].hostPort(containerPort)
	if !ok {
		// An exited container reports no port bindings, so a model that fails
		// immediately surfaces here rather than in waitLoaded.
		if !infos[0].State.Running {
			return nil, exitedFailure(ctx, engine, id, infos[0]), nil
		}
		return nil, nil, fmt.Errorf("%s did not publish port 8080", name)
	}
	session.BaseURL = "http://127.0.0.1:" + port

	started := time.Now()
	status.printf("loading %s (this can take several minutes for large models)", plan.ModelName)
	failure, err = waitLoaded(ctx, engine, id, session.BaseURL, loadTimeout, client)
	if err != nil {
		return nil, nil, err
	}
	if failure != nil {
		return nil, failure, nil
	}
	status.printf("%s loaded in %s", plan.ModelName, time.Since(started).Round(time.Second))

	if err := verifyServedContext(ctx, client, session.BaseURL, plan.ModelName, decision.Tokens); err != nil {
		var mismatch *contextMismatchError
		if errors.As(err, &mismatch) {
			return nil, nil, err
		}
		status.printf("warning: could not verify the served context: %v", err)
	}
	if engine.Accel != AccelNone {
		if loaded, total, ok := gpuOffload(engine.combinedLogs(ctx, id, 2000)); ok && loaded < total {
			status.printf("warning: only %d/%d layers fit on the GPU at context %d; generation will be slow. Try a smaller --context-size", loaded, total, decision.Tokens)
		}
	}
	succeeded = true
	return session, nil, nil
}

func waitLoaded(ctx context.Context, engine *Engine, id, baseURL string, timeout time.Duration, client *http.Client) (*loadFailure, error) {
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readyOK(ctx, client, baseURL) {
			return nil, nil
		}
		if attempt%statePollEvery == 0 {
			infos, err := engine.inspect(ctx, id)
			if err != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err == nil && len(infos) > 0 && !infos[0].State.Running {
				return exitedFailure(ctx, engine, id, infos[0]), nil
			}
		}
		if time.Now().After(deadline) {
			return &loadFailure{timedOut: true, logs: engine.combinedLogs(context.WithoutCancel(ctx), id, logTailLines)}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

func exitedFailure(ctx context.Context, engine *Engine, id string, info ContainerInfo) *loadFailure {
	logs := engine.combinedLogs(context.WithoutCancel(ctx), id, logTailLines)
	return &loadFailure{
		exitCode:  info.State.ExitCode,
		oomKilled: info.State.OOMKilled,
		oom:       info.State.OOMKilled || info.State.ExitCode == 137 || looksLikeOOM(logs),
		logs:      logs,
	}
}

func readyOK(ctx context.Context, client *http.Client, baseURL string) bool {
	requestCtx, cancel := context.WithTimeout(ctx, readyRequestLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

var oomPatterns = []string{
	"out of memory",
	"failed to allocate",
	"unable to allocate",
	"cannot allocate memory",
	"std::bad_alloc",
	"erroroutofdevicememory",
	"erroroutofhostmemory",
	"cudamalloc failed",
	"failed to create context",
}

func looksLikeOOM(logs string) bool {
	lower := strings.ToLower(logs)
	for _, pattern := range oomPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

var offloadPattern = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)

func gpuOffload(logs string) (int, int, bool) {
	matches := offloadPattern.FindAllStringSubmatch(logs, -1)
	if len(matches) == 0 {
		return 0, 0, false
	}
	last := matches[len(matches)-1]
	loaded, err1 := strconv.Atoi(last[1])
	total, err2 := strconv.Atoi(last[2])
	if err1 != nil || err2 != nil || total == 0 {
		return 0, 0, false
	}
	return loaded, total, true
}

type contextMismatchError struct {
	want, got int
}

func (e *contextMismatchError) Error() string {
	return fmt.Sprintf("LocalAI reports context_size %d, not the requested %d; the image may fix context_size in a config vekil did not read", e.got, e.want)
}

// verifyServedContext reads the model config LocalAI applied.
func verifyServedContext(ctx context.Context, client *http.Client, baseURL, model string, want int) error {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/api/models/config-json/"+url.PathEscape(model), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return err
	}
	raw, ok := payload["context_size"]
	if !ok {
		if nested, found := payload["config"]; found {
			var inner map[string]json.RawMessage
			if json.Unmarshal(nested, &inner) == nil {
				raw, ok = inner["context_size"]
			}
		}
	}
	if !ok {
		return fmt.Errorf("response has no context_size")
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("parse context_size: %w", err)
	}
	if got != want {
		return &contextMismatchError{want: want, got: got}
	}
	return nil
}

func findReusable(ctx context.Context, engine *Engine, spec string, client *http.Client) *Session {
	infos, err := engine.listByLabel(ctx, LabelSpec+"="+spec)
	if err != nil {
		return nil
	}
	for _, info := range infos {
		labels := info.Config.Labels
		if labels[LabelKeep] != "true" {
			continue
		}
		if !info.State.Running {
			_ = engine.remove(ctx, info.ID)
			continue
		}
		port, ok := info.hostPort(containerPort)
		if !ok {
			continue
		}
		baseURL := "http://127.0.0.1:" + port
		if !readyOK(ctx, client, baseURL) {
			continue
		}
		contextTokens, _ := strconv.Atoi(labels[LabelContext])
		name := strings.TrimPrefix(info.Name, "/")
		if name == "" {
			name = info.ID
		}
		return &Session{
			engine:        engine,
			ContainerID:   info.ID,
			ContainerName: name,
			BaseURL:       baseURL,
			ModelName:     labels[LabelModel],
			Backend:       labels[LabelBackend],
			ContextTokens: contextTokens,
			Reused:        true,
			Keep:          true,
		}
	}
	return nil
}

// reapOrphans removes containers left by vekil processes on this host that are
// no longer running and reports removals that fail. Kept containers are left
// alone. A failed listing is ignored; the next engine command reports it.
func reapOrphans(ctx context.Context, engine *Engine) error {
	infos, err := engine.listByLabel(ctx, LabelManaged)
	if err != nil {
		return nil
	}
	var errs []error
	host := hostName()
	for _, info := range infos {
		labels := info.Config.Labels
		if labels[LabelKeep] == "true" {
			continue
		}
		ownerHost, pidText, found := strings.Cut(labels[LabelOwner], "/")
		if !found || ownerHost != host {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid == os.Getpid() || processAlive(pid) {
			continue
		}
		if err := engine.remove(ctx, info.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", info.ID, err))
		}
	}
	return errors.Join(errs...)
}

func ownerLabel() string { return hostName() + "/" + strconv.Itoa(os.Getpid()) }

func hostName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown"
	}
	return strings.ReplaceAll(strings.TrimSpace(name), "/", "_")
}

func specHash(parts ...interface{}) string {
	digest := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(digest, "%v\x00", part)
	}
	return hex.EncodeToString(digest.Sum(nil))[:24]
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:6])
}

func randomSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(buf[:])
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}

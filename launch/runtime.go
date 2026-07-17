package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	defaultStartupTimeout   = 2 * time.Minute
	defaultShutdownTimeout  = 10 * time.Second
	defaultChildStopTimeout = 3 * time.Second
	readyRequestTimeout     = 15 * time.Second
	modelRequestTimeout     = 15 * time.Second
	maxLauncherResponseBody = 8 << 20
)

type modelsResponse struct {
	Data []ModelInfo `json:"data"`
}

type statsTotals struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

type statsBreakdown struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
	Errors   int64  `json:"errors"`
}

type statsSnapshot struct {
	Totals     statsTotals      `json:"totals"`
	ByModel    []statsBreakdown `json:"by_model"`
	ByProvider []statsBreakdown `json:"by_provider"`
}

type commandOutcome struct {
	code int
	err  error
}

type launchSignalCause struct {
	signal os.Signal
}

func (e *launchSignalCause) Error() string {
	return fmt.Sprintf("received signal %v", e.signal)
}

type startupOperationResult[T any] struct {
	value T
	err   error
}

// Run starts proxy, validates the requested model, prepares the agent process,
// supervises it, prints a session summary, and tears everything down.
func Run(parent context.Context, proxy Proxy, adapter Adapter, opts Options) (result Result, returnErr error) {
	if parent == nil {
		parent = context.Background()
	}
	if adapter == nil {
		return result, fmt.Errorf("launch adapter is nil")
	}
	opts = normalizeOptions(opts)
	modelID := strings.TrimSpace(opts.Model)
	if modelID == "" {
		return result, fmt.Errorf("launch %s requires --model", adapter.Name())
	}
	localToken := strings.TrimSpace(opts.LocalToken)
	if opts.DryRun && localToken == "" {
		localToken = "placeholder"
	}
	if !opts.DryRun && localToken == "" {
		return result, fmt.Errorf("launch %s requires a local proxy token", adapter.Name())
	}

	runCtx, cancelRun := context.WithCancelCause(parent)
	defer cancelRun(context.Canceled)
	if opts.Signals != nil {
		go func() {
			select {
			case signalValue, ok := <-opts.Signals:
				if ok && signalValue != nil {
					cancelRun(&launchSignalCause{signal: signalValue})
				}
			case <-runCtx.Done():
			}
		}()
	}

	if opts.DryRun {
		baseURL := strings.TrimRight(strings.TrimSpace(opts.DryRunBaseURL), "/")
		if baseURL == "" {
			baseURL = dryRunBaseURL
		}
		model := ModelInfo{ID: modelID, Name: modelID}
		if opts.DryRunModel != nil {
			model = *opts.DryRunModel
			resolvedID := strings.TrimSpace(model.ID)
			if resolvedID == "" {
				resolvedID = modelID
			}
			if resolvedID != modelID {
				return result, fmt.Errorf("dry-run model metadata is for %q, want %q", resolvedID, modelID)
			}
			model.ID = resolvedID
			if strings.TrimSpace(model.Name) == "" {
				model.Name = resolvedID
			}
		}
		prepared, err := adapter.Prepare(PrepareInput{
			BaseURL:       baseURL,
			Model:         model,
			Binary:        opts.Binary,
			ForwardedArgs: opts.ForwardedArgs,
			LocalToken:    localToken,
			SensitiveEnv:  opts.SensitiveEnv,
			Environment:   opts.Environment,
			NoProxy:       loopbackNoProxyValue(opts.Environment, baseURL),
			DryRun:        true,
		})
		if err != nil {
			return result, err
		}
		defer cleanupPreparedProcess(prepared, opts.Stderr)
		printDryRun(opts.Stderr, adapter.Name(), modelID, baseURL, opts.SensitiveEnv, prepared)
		return Result{ExitCode: 0, BaseURL: baseURL, Model: model}, nil
	}
	if proxy == nil {
		return result, fmt.Errorf("launch proxy is nil")
	}

	startupCtx, startupCancel := context.WithTimeout(runCtx, opts.StartupTimeout)
	defer startupCancel()
	if err := proxy.Start(startupCtx); err != nil {
		if runCtx.Err() != nil {
			result.ExitCode = cancellationExitCode(runCtx)
			return result, nil
		}
		return result, err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cancel()
		if err := proxy.Stop(stopCtx); err != nil {
			if returnErr == nil && result.ExitCode == 0 {
				returnErr = fmt.Errorf("stop launch proxy: %w", err)
			} else {
				_, _ = fmt.Fprintf(opts.Stderr, "vekil: warning: proxy shutdown failed: %v\n", err)
			}
		}
	}()
	if startupCtx.Err() != nil {
		if runCtx.Err() != nil {
			result.ExitCode = cancellationExitCode(runCtx)
			return result, nil
		}
		return result, startupCtx.Err()
	}

	baseURL, err := proxyBaseURL(proxy.Addr())
	if err != nil {
		return result, err
	}
	result.BaseURL = baseURL

	_, err = runStartupOperation(startupCtx, proxy.Done(), func(ctx context.Context) (struct{}, error) {
		return struct{}{}, waitForReady(ctx, baseURL, opts.StartupTimeout)
	})
	if err != nil {
		if runCtx.Err() != nil {
			result.ExitCode = cancellationExitCode(runCtx)
			return result, nil
		}
		return result, err
	}
	models, err := runStartupOperation(startupCtx, proxy.Done(), func(ctx context.Context) ([]ModelInfo, error) {
		return fetchModels(ctx, baseURL, localToken)
	})
	if err != nil {
		if runCtx.Err() != nil {
			result.ExitCode = cancellationExitCode(runCtx)
			return result, nil
		}
		return result, err
	}
	model, err := selectModel(models, modelID)
	if err != nil {
		return result, err
	}
	result.Model = model
	startupCancel()

	prepared, err := adapter.Prepare(PrepareInput{
		BaseURL:       baseURL,
		Model:         model,
		Binary:        opts.Binary,
		ForwardedArgs: opts.ForwardedArgs,
		LocalToken:    localToken,
		SensitiveEnv:  opts.SensitiveEnv,
		Environment:   opts.Environment,
		NoProxy:       loopbackNoProxyValue(opts.Environment, baseURL),
	})
	if err != nil {
		return result, err
	}
	defer cleanupPreparedProcess(prepared, opts.Stderr)

	printReadyBanner(opts.Stderr, adapter.Name(), model, baseURL, opts.LogPath)
	cmd := exec.Command(prepared.Path, prepared.Args...)
	cmd.Env = applyEnvironment(
		opts.Environment,
		append(append([]string(nil), opts.SensitiveEnv...), prepared.EnvUnset...),
		prepared.EnvSet,
	)
	cmd.Env = ensureLoopbackNoProxy(cmd.Env, baseURL)
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.WaitDelay = opts.ChildStopTimeout

	controller, err := newProcessController(cmd, opts.Stdin)
	if err != nil {
		return result, fmt.Errorf("configure %s process: %w", adapter.Name(), err)
	}
	defer func() {
		if err := controller.close(); err != nil {
			_, _ = fmt.Fprintf(opts.Stderr, "vekil: warning: restore launcher process state: %v\n", err)
		}
	}()
	if runCtx.Err() != nil {
		result.ExitCode = cancellationExitCode(runCtx)
		return result, nil
	}
	select {
	case proxyErr, ok := <-proxy.Done():
		return result, fmt.Errorf(
			"launch proxy failed before agent start: %w",
			normalizedProxyDoneError(proxyErr, ok),
		)
	default:
	}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start %s: %w", adapter.Name(), err)
	}
	if err := controller.afterStart(); err != nil {
		return result, reapFailedContainedCommand(controller, fmt.Errorf("initialize %s process: %w", adapter.Name(), err))
	}

	waitCh := make(chan commandOutcome, 1)
	go func() {
		waitCh <- waitAndCloseController(controller)
	}()

	select {
	case outcome := <-waitCh:
		result.ExitCode = outcome.code
		proxyErr := pollProxyFailure(proxy.Done())
		if outcome.err != nil || proxyErr != nil {
			return result, errors.Join(outcome.err, proxyErr)
		}
	case <-runCtx.Done():
		signalValue := signalFromContext(runCtx)
		if signalValue == nil {
			signalValue = os.Interrupt
		}
		outcome := stopCommand(controller, waitCh, opts.ChildStopTimeout, signalValue, true)
		result.ExitCode = cancellationExitCode(runCtx)
		if outcome.err != nil {
			return result, outcome.err
		}
	case proxyErr, ok := <-proxy.Done():
		proxyErr = normalizedProxyDoneError(proxyErr, ok)
		outcome := stopCommand(controller, waitCh, opts.ChildStopTimeout, os.Interrupt, false)
		result.ExitCode = outcome.code
		return result, errors.Join(fmt.Errorf("launch proxy failed: %w", proxyErr), outcome.err)
	}

	if !opts.NoSummary {
		if snapshot, err := fetchStats(context.Background(), baseURL, localToken); err == nil {
			printSessionSummary(opts.Stderr, snapshot)
		}
	}
	return result, nil
}

func pollProxyFailure(done <-chan error) error {
	select {
	case proxyErr, ok := <-done:
		return fmt.Errorf("launch proxy failed: %w", normalizedProxyDoneError(proxyErr, ok))
	default:
		return nil
	}
}

const dryRunBaseURL = "http://127.0.0.1:<dynamic>"

func normalizeOptions(opts Options) Options {
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = defaultStartupTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	if opts.ChildStopTimeout <= 0 {
		opts.ChildStopTimeout = defaultChildStopTimeout
	}
	if opts.Environment == nil {
		opts.Environment = os.Environ()
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	return opts
}

func signalFromContext(ctx context.Context) os.Signal {
	var signalCause *launchSignalCause
	if errors.As(context.Cause(ctx), &signalCause) {
		return signalCause.signal
	}
	return nil
}

func cancellationExitCode(ctx context.Context) int {
	if signalValue := signalFromContext(ctx); signalValue != nil {
		return processSignalExitCode(signalValue)
	}
	return 130
}

func normalizedProxyDoneError(err error, ok bool) error {
	if !ok || err == nil {
		return errors.New("proxy stopped unexpectedly")
	}
	return err
}

func runStartupOperation[T any](
	ctx context.Context,
	proxyDone <-chan error,
	operation func(context.Context) (T, error),
) (T, error) {
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resultCh := make(chan startupOperationResult[T], 1)
	go func() {
		value, err := operation(operationCtx)
		resultCh <- startupOperationResult[T]{value: value, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.value, result.err
	case proxyErr, ok := <-proxyDone:
		cancel()
		<-resultCh
		var zero T
		return zero, fmt.Errorf("launch proxy failed before agent start: %w", normalizedProxyDoneError(proxyErr, ok))
	case <-ctx.Done():
		cancel()
		result := <-resultCh
		if result.err != nil {
			return result.value, result.err
		}
		var zero T
		return zero, context.Cause(ctx)
	}
}

func proxyBaseURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("launch proxy did not publish a listen address")
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/"), nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse launch proxy address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func waitForReady(ctx context.Context, baseURL string, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{}
	var lastDetail string
	for {
		requestCtx, requestCancel := context.WithTimeout(readyCtx, readyRequestTimeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/readyz", nil)
		if err != nil {
			requestCancel()
			return err
		}
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				requestCancel()
				return nil
			}
			lastDetail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		} else {
			lastDetail = requestErr.Error()
		}
		requestCancel()

		select {
		case <-readyCtx.Done():
			if lastDetail == "" {
				lastDetail = readyCtx.Err().Error()
			}
			return fmt.Errorf("launch proxy did not become ready within %s: %s", timeout, lastDetail)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func fetchModels(ctx context.Context, baseURL, localToken string) ([]ModelInfo, error) {
	requestCtx, cancel := context.WithTimeout(ctx, modelRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch launch models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch launch models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload modelsResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxLauncherResponseBody))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode launch models: %w", err)
	}
	return payload.Data, nil
}

func selectModel(models []ModelInfo, id string) (ModelInfo, error) {
	for _, model := range models {
		if model.ID == id {
			return model, nil
		}
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) != "" {
			ids = append(ids, model.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) > 12 {
		ids = append(ids[:12], "…")
	}
	if len(ids) == 0 {
		return ModelInfo{}, fmt.Errorf("model %q is not served by this Vekil instance; /v1/models returned no models", id)
	}
	return ModelInfo{}, fmt.Errorf("model %q is not served by this Vekil instance; available models: %s", id, strings.Join(ids, ", "))
}

func waitCommand(cmd *exec.Cmd) commandOutcome {
	err := cmd.Wait()
	if err == nil {
		return commandOutcome{code: 0}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return commandOutcome{code: processStateExitCode(exitErr.ProcessState)}
	}
	return commandOutcome{code: 1, err: fmt.Errorf("wait for agent process: %w", err)}
}

func stopCommand(
	controller processController,
	waitCh <-chan commandOutcome,
	timeout time.Duration,
	signalValue os.Signal,
	terminalSignalLikely bool,
) commandOutcome {
	if terminalSignalLikely {
		select {
		case outcome := <-waitCh:
			return outcome
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = controller.signal(signalValue)
	select {
	case outcome := <-waitCh:
		return outcome
	case <-time.After(timeout):
	}
	killErr := controller.kill()
	select {
	case outcome := <-waitCh:
		outcome.err = errors.Join(outcome.err, killErr)
		return outcome
	case <-time.After(timeout):
		return commandOutcome{
			code: 1,
			err: errors.Join(
				killErr,
				fmt.Errorf("agent process did not exit within %s after forced termination", timeout),
			),
		}
	}
}

func cleanupPreparedProcess(prepared PreparedProcess, stderr io.Writer) {
	if prepared.Cleanup == nil {
		return
	}
	if err := prepared.Cleanup(); err != nil {
		_, _ = fmt.Fprintf(stderr, "vekil: warning: launcher cleanup failed: %v\n", err)
	}
}

func printReadyBanner(w io.Writer, agent string, model ModelInfo, baseURL, logPath string) {
	_, _ = fmt.Fprintf(w, "\nvekil ready: %s -> %s\n", agent, model.ID)
	_, _ = fmt.Fprintf(w, "  proxy:     %s\n", baseURL)
	if strings.TrimSpace(logPath) != "" {
		_, _ = fmt.Fprintf(w, "  proxy log: %s\n", logPath)
	}
	_, _ = fmt.Fprintln(w)
}

func printDryRun(
	w io.Writer,
	agent string,
	model string,
	baseURL string,
	sensitiveEnv []string,
	prepared PreparedProcess,
) {
	_, _ = fmt.Fprintln(w, "vekil launch dry-run")
	_, _ = fmt.Fprintf(w, "  agent:  %s\n", agent)
	_, _ = fmt.Fprintf(w, "  model:  %s\n", model)
	_, _ = fmt.Fprintf(w, "  proxy:  %s\n", baseURL)
	_, _ = fmt.Fprintf(w, "  binary: %s\n", prepared.Path)
	if len(prepared.Args) > 0 {
		_, _ = fmt.Fprintf(w, "  args:   %q\n", prepared.Args)
	}
	unresolved := append([]string(nil), prepared.Unresolved...)
	sort.Strings(unresolved)
	for _, item := range unresolved {
		if item = strings.TrimSpace(item); item != "" {
			_, _ = fmt.Fprintf(w, "  unresolved: %s\n", item)
		}
	}

	setKeys := make([]string, 0, len(prepared.EnvSet))
	for key := range prepared.EnvSet {
		setKeys = append(setKeys, key)
	}
	sort.Strings(setKeys)
	for _, key := range setKeys {
		value := prepared.EnvSet[key]
		if strings.Contains(strings.ToUpper(key), "KEY") || strings.Contains(strings.ToUpper(key), "TOKEN") {
			if value == "" {
				value = "[empty]"
			} else {
				value = "[local placeholder]"
			}
		}
		_, _ = fmt.Fprintf(w, "  set:    %s=%s\n", key, value)
	}
	_, _ = fmt.Fprintln(w, "  append: NO_PROXY/no_proxy += launcher loopback host, localhost, 127.0.0.1, ::1")

	unsetSeen := make(map[string]struct{}, len(sensitiveEnv)+len(prepared.EnvUnset))
	unset := make([]string, 0, len(sensitiveEnv)+len(prepared.EnvUnset))
	for _, key := range append(append([]string(nil), sensitiveEnv...), prepared.EnvUnset...) {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		canonical := canonicalEnvironmentKey(key)
		if _, ok := unsetSeen[canonical]; ok {
			continue
		}
		setByAdapter := false
		for setKey := range prepared.EnvSet {
			if canonicalEnvironmentKey(setKey) == canonical {
				setByAdapter = true
				break
			}
		}
		if setByAdapter {
			continue
		}
		unsetSeen[canonical] = struct{}{}
		unset = append(unset, key)
	}
	sort.Strings(unset)
	for _, key := range unset {
		_, _ = fmt.Fprintf(w, "  unset:  %s\n", key)
	}
}

func fetchStats(ctx context.Context, baseURL, localToken string) (statsSnapshot, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/stats.json", nil)
	if err != nil {
		return statsSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return statsSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statsSnapshot{}, fmt.Errorf("stats returned HTTP %d", resp.StatusCode)
	}
	var snapshot statsSnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLauncherResponseBody)).Decode(&snapshot); err != nil {
		return statsSnapshot{}, err
	}
	return snapshot, nil
}

func printSessionSummary(w io.Writer, snapshot statsSnapshot) {
	if snapshot.Totals.Requests == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "vekil session summary")
	_, _ = fmt.Fprintf(w, "  requests: %d", snapshot.Totals.Requests)
	if snapshot.Totals.Errors > 0 {
		_, _ = fmt.Fprintf(w, " (%d errors)", snapshot.Totals.Errors)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(
		w,
		"  tokens:   %d in, %d out",
		snapshot.Totals.PromptTokens,
		snapshot.Totals.CompletionTokens,
	)
	if snapshot.Totals.CachedTokens > 0 {
		_, _ = fmt.Fprintf(w, ", %d cached", snapshot.Totals.CachedTokens)
	}
	if snapshot.Totals.ReasoningTokens > 0 {
		_, _ = fmt.Fprintf(w, ", %d reasoning", snapshot.Totals.ReasoningTokens)
	}
	_, _ = fmt.Fprintln(w)
	for _, row := range snapshot.ByModel {
		if row.Model == "" || row.Requests == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "  model:    %s (%d requests, %d tokens", row.Model, row.Requests, row.Tokens)
		if row.Errors > 0 {
			_, _ = fmt.Fprintf(w, ", %d errors", row.Errors)
		}
		_, _ = fmt.Fprintln(w, ")")
	}
}

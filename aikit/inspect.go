package aikit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	localAIConfigPath   = "/config.yaml"
	localAIModelsPath   = "/models"
	maxLocalAIConfigLen = 4 << 20
	remoteHeaderChunk   = 4 << 20
	maxRemoteConfigLen  = 4 << 20
)

// modelPlan is everything Start needs to know about the model before it runs a
// container.
type modelPlan struct {
	Image     string
	Backend   string
	ModelName string
	// TrainedContext is the model's trained context, or zero when unknown.
	TrainedContext int
	// PinnedContext is a context_size fixed by the image's config, or zero.
	PinnedContext int
	// PoolTokens is a vllm-cpp KV pool size fixed by the image's config, or zero.
	PoolTokens int
	// WeightsBytes is the size of the main weights file, or zero when unknown.
	WeightsBytes int64
	// RunnerArgs are appended after the image on the run command line.
	RunnerArgs []string
	// CacheVolume is mounted at /models for runner images.
	CacheVolume string
}

// localAIModelConfig is the subset of a LocalAI model config entry vekil reads.
type localAIModelConfig struct {
	Name        string   `yaml:"name"`
	Backend     string   `yaml:"backend"`
	ContextSize *int     `yaml:"context_size"`
	Options     []string `yaml:"options"`
	Parameters  struct {
		Model string `yaml:"model"`
	} `yaml:"parameters"`
}

func parseLocalAIConfig(data []byte) ([]localAIModelConfig, error) {
	var list []localAIModelConfig
	if err := yaml.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var single localAIModelConfig
	if err := yaml.Unmarshal(data, &single); err != nil {
		return nil, fmt.Errorf("parse LocalAI config: %w", err)
	}
	if strings.TrimSpace(single.Name) == "" {
		return nil, fmt.Errorf("LocalAI config has no model entries")
	}
	return []localAIModelConfig{single}, nil
}

func normalizeBackend(backend string) (string, bool) {
	switch strings.TrimSpace(strings.ToLower(backend)) {
	case "", "llama-cpp", "llama", "llama.cpp":
		return BackendLlamaCPP, true
	case "vllm-cpp", "vllm.cpp":
		return BackendVLLMCPP, true
	default:
		return strings.TrimSpace(backend), false
	}
}

// selectChatModel picks the one servable text model from a LocalAI config.
func selectChatModel(entries []localAIModelConfig, selector string) (localAIModelConfig, string, error) {
	selector = strings.TrimSpace(selector)
	var candidates []localAIModelConfig
	var backends []string
	var names []string
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
		backend, supported := normalizeBackend(entry.Backend)
		if selector != "" {
			if name == selector {
				if !supported {
					return localAIModelConfig{}, "", fmt.Errorf("model %q uses backend %q; vekil serves llama-cpp and vllm-cpp models", name, entry.Backend)
				}
				return entry, backend, nil
			}
			continue
		}
		if supported {
			candidates = append(candidates, entry)
			backends = append(backends, backend)
		}
	}
	if selector != "" {
		return localAIModelConfig{}, "", fmt.Errorf("model %q is not in the image config (models: %s)", selector, strings.Join(names, ", "))
	}
	switch len(candidates) {
	case 0:
		return localAIModelConfig{}, "", fmt.Errorf("image config has no llama-cpp or vllm-cpp text model (models: %s)", strings.Join(names, ", "))
	case 1:
		return candidates[0], backends[0], nil
	default:
		candidateNames := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			candidateNames = append(candidateNames, candidate.Name)
		}
		return localAIModelConfig{}, "", fmt.Errorf("image config has several text models (%s); select one with <ref>#<name>", strings.Join(candidateNames, ", "))
	}
}

// vllmPoolTokens returns the KV pool a vllm-cpp config fixes through its
// options, or zero when the engine default (256 blocks of 32 tokens) applies.
func vllmPoolTokens(options []string) int {
	blockSize := 32
	numBlocks := 0
	for _, option := range options {
		key, value, found := strings.Cut(option, ":")
		if !found {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed <= 0 {
			continue
		}
		switch strings.TrimSpace(key) {
		case "block_size":
			blockSize = parsed
		case "num_blocks":
			numBlocks = parsed
		}
	}
	if numBlocks <= 1 {
		return 0
	}
	// vllm.cpp reserves block 0 as the null block.
	return (numBlocks - 1) * blockSize
}

func inspectImage(ctx context.Context, engine *Engine, image, selector string) (modelPlan, error) {
	probe, err := engine.create(ctx, "--label", LabelManaged+"=probe", "--label", LabelOwner+"="+ownerLabel(), image)
	if err != nil {
		return modelPlan{}, fmt.Errorf("create probe container for %s: %w", image, err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if engine.remove(removeCtx, probe) != nil {
			rememberUnremoved(engine, probe)
		}
	}()

	configReader, _, configCloser, err := engine.copyFileOut(ctx, probe, localAIConfigPath)
	if err != nil {
		return modelPlan{}, fmt.Errorf("read %s from %s (runner images take aikit:hf.co/... or an https .gguf URL): %w", localAIConfigPath, image, err)
	}
	configData, err := io.ReadAll(io.LimitReader(configReader, maxLocalAIConfigLen+1))
	_ = configCloser.Close()
	if err != nil {
		return modelPlan{}, fmt.Errorf("read %s from %s: %w", localAIConfigPath, image, err)
	}
	if len(configData) > maxLocalAIConfigLen {
		return modelPlan{}, fmt.Errorf("%s in %s is larger than %d bytes", localAIConfigPath, image, maxLocalAIConfigLen)
	}
	entries, err := parseLocalAIConfig(configData)
	if err != nil {
		return modelPlan{}, fmt.Errorf("%s in %s: %w", localAIConfigPath, image, err)
	}
	entry, backend, err := selectChatModel(entries, selector)
	if err != nil {
		return modelPlan{}, fmt.Errorf("%s: %w", image, err)
	}
	plan := modelPlan{Image: image, Backend: backend, ModelName: strings.TrimSpace(entry.Name)}
	if entry.ContextSize != nil && *entry.ContextSize > 0 {
		plan.PinnedContext = *entry.ContextSize
	}
	if backend == BackendVLLMCPP {
		plan.PoolTokens = vllmPoolTokens(entry.Options)
	}

	modelFile := strings.TrimSpace(entry.Parameters.Model)
	if modelFile == "" {
		return plan, nil
	}
	if !path.IsAbs(modelFile) {
		modelFile = path.Join(localAIModelsPath, modelFile)
	}
	if !strings.HasSuffix(strings.ToLower(modelFile), ".gguf") {
		return plan, nil
	}
	weights, size, closer, err := engine.copyFileOut(ctx, probe, modelFile)
	if err != nil {
		return modelPlan{}, fmt.Errorf("read model file %s from %s: %w", modelFile, image, err)
	}
	info, infoErr := ReadGGUFInfo(weights)
	_ = closer.Close()
	plan.WeightsBytes = size
	if infoErr != nil && !errors.Is(infoErr, errGGUFNotFound) {
		return modelPlan{}, fmt.Errorf("read GGUF metadata from %s: %w", modelFile, infoErr)
	}
	plan.TrainedContext = clampContext(info.ContextLength)
	return plan, nil
}

func clampContext(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > 1<<30 {
		return 1 << 30
	}
	return int(value)
}

// huggingFaceToken returns HF_TOKEN from environment, if set.
func huggingFaceToken(environment []string) string {
	for i := len(environment) - 1; i >= 0; i-- {
		key, value, found := strings.Cut(environment[i], "=")
		if found && key == "HF_TOKEN" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// rangeReader reads a remote file sequentially through HTTP range requests so
// only the bytes the GGUF parser consumes are transferred.
type rangeReader struct {
	ctx    context.Context
	client *http.Client
	url    string
	token  string
	offset int64
	buf    []byte
	size   int64
	err    error
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
		if len(r.buf) == 0 {
			if r.err == nil {
				r.err = io.EOF
			}
			return 0, r.err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *rangeReader) fill() {
	if r.size > 0 && r.offset >= r.size {
		r.err = io.EOF
		return
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		r.err = err
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.offset, r.offset+remoteHeaderChunk-1))
	req.Header.Set("User-Agent", "vekil-aikit")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.err = fmt.Errorf("fetch %s: %w", redactURL(r.url), redactURLError(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if total := contentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			r.size = total
		}
	case http.StatusOK:
		// The server ignored the range; read only the first chunk.
		if r.offset > 0 {
			r.err = fmt.Errorf("fetch %s: server does not support range requests", redactURL(r.url))
			return
		}
		if resp.ContentLength > 0 {
			r.size = resp.ContentLength
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		r.err = fmt.Errorf("fetch %s: HTTP %d (gated models need HF_TOKEN)", redactURL(r.url), resp.StatusCode)
		return
	default:
		r.err = fmt.Errorf("fetch %s: HTTP %d", redactURL(r.url), resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, remoteHeaderChunk))
	if err != nil {
		r.err = fmt.Errorf("fetch %s: %w", redactURL(r.url), err)
		return
	}
	r.offset += int64(len(data))
	r.buf = data
}

func contentRangeTotal(value string) int64 {
	_, total, found := strings.Cut(value, "/")
	if !found {
		return 0
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func redactURL(raw string) string {
	if index := strings.IndexAny(raw, "?#"); index >= 0 {
		return raw[:index]
	}
	return raw
}

// redactURLError strips the query and credentials from the URL a failed
// request reports. After a redirect, that URL can be a signed download link.
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return &url.Error{Op: urlErr.Op, URL: displayURL(urlErr.URL), Err: urlErr.Err}
}

// secureRedirects copies client with a redirect policy that never leaves
// https, so credentials on the first request cannot follow a downgrade.
func secureRedirects(client *http.Client) *http.Client {
	secured := *client
	previous := client.CheckRedirect
	secured.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect from https to %s", req.URL.Scheme)
		}
		// The runner container cannot reach this machine's loopback, so a
		// redirect there would inspect a file the runner cannot download.
		if loopbackHost(req.URL.Hostname()) {
			return fmt.Errorf("refusing redirect to %s, which the runner container cannot reach", req.URL.Hostname())
		}
		if previous != nil {
			return previous(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &secured
}

func inspectRemoteGGUF(ctx context.Context, client *http.Client, ref Reference, token string) (modelPlan, error) {
	reader := &rangeReader{ctx: ctx, client: secureRedirects(client), url: ref.Source, token: token}
	info, err := ReadGGUFInfo(reader)
	if err != nil && !errors.Is(err, errGGUFNotFound) {
		return modelPlan{}, fmt.Errorf("read GGUF metadata from %s: %w", redactURL(ref.Source), err)
	}
	return modelPlan{
		Backend:        BackendLlamaCPP,
		ModelName:      ref.ModelName,
		TrainedContext: clampContext(info.ContextLength),
		WeightsBytes:   reader.size,
	}, nil
}

func inspectRemoteRepository(ctx context.Context, client *http.Client, ref Reference, token string) (modelPlan, error) {
	configURL := "https://huggingface.co/" + ref.Repository + "/resolve/" + ref.Revision + "/config.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL, nil)
	if err != nil {
		return modelPlan{}, err
	}
	req.Header.Set("User-Agent", "vekil-aikit")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := secureRedirects(client).Do(req)
	if err != nil {
		return modelPlan{}, fmt.Errorf("fetch %s: %w", configURL, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return modelPlan{}, fmt.Errorf("fetch %s: HTTP %d", configURL, resp.StatusCode)
	}
	var config struct {
		MaxPositionEmbeddings int64 `json:"max_position_embeddings"`
		TextConfig            struct {
			MaxPositionEmbeddings int64 `json:"max_position_embeddings"`
		} `json:"text_config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRemoteConfigLen)).Decode(&config); err != nil {
		return modelPlan{}, fmt.Errorf("parse %s: %w", configURL, err)
	}
	trained := config.MaxPositionEmbeddings
	if trained <= 0 {
		trained = config.TextConfig.MaxPositionEmbeddings
	}
	return modelPlan{
		Backend:        BackendVLLMCPP,
		ModelName:      ref.ModelName,
		TrainedContext: clampContext(trained),
	}, nil
}

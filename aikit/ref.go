// Package aikit runs AIKit model images as local, launcher-owned LocalAI
// servers and describes them to Vekil as ordinary OpenAI-compatible providers.
//
// The proxy never manages containers. Callers start a Session before the proxy
// is constructed, then route to the session's loopback base URL.
package aikit

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Prefix marks a launcher model reference that names an AIKit model.
const Prefix = "aikit:"

// PremadeRegistry is where AIKit publishes its pre-made model images.
const PremadeRegistry = "ghcr.io/kaito-project/aikit"

// RefKind identifies how a reference is served.
type RefKind int

const (
	// RefImage is an AIKit model image with its model baked in.
	RefImage RefKind = iota
	// RefRunnerGGUF is a direct HTTP(S) GGUF file served by a runner image.
	RefRunnerGGUF
	// RefRunnerRepo is a Hugging Face repository served by a runner image.
	RefRunnerRepo
)

// Backend names a LocalAI backend family.
const (
	BackendLlamaCPP = "llama-cpp"
	BackendVLLMCPP  = "vllm-cpp"
)

// Reference is a parsed AIKit model reference.
type Reference struct {
	// Raw is the reference without the aikit: prefix.
	Raw  string
	Kind RefKind
	// Image is the model image for RefImage references.
	Image string
	// Premade reports a short pre-made name such as qwen3.8:27b.
	Premade bool
	// Selector picks one model from an image config that holds several
	// (image#model).
	Selector string
	// Source is the runner model argument for runner references.
	Source string
	// ModelName is the LocalAI model name a runner derives from Source.
	ModelName string
	// Repository and Revision describe RefRunnerRepo references.
	Repository string
	Revision   string
}

var (
	premadeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
	premadeTagPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	hfSegmentPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	hfCommitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ggufFilePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.gguf$`)
	selectorPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// HasPrefix reports whether value is an aikit: model reference.
func HasPrefix(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), Prefix)
}

// ParsePrefixed parses an aikit:<ref> value.
func ParsePrefixed(value string) (Reference, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, Prefix) {
		return Reference{}, fmt.Errorf("model reference %q does not start with %q", value, Prefix)
	}
	return ParseReference(strings.TrimPrefix(value, Prefix))
}

// ParseReference parses a reference without the aikit: prefix.
//
// Accepted forms:
//   - name:tag, a pre-made image under ghcr.io/kaito-project/aikit;
//   - a full image reference whose first component is a registry host;
//   - https://.../file.gguf, a GGUF file served by a runner image;
//   - hf.co/org/repo/path/file.gguf, shorthand for the file on the main revision;
//   - hf.co/org/repo@<40-hex commit>, a repository served by a runner image.
//
// Image references may end in #model to select one model from an image whose
// config holds several.
func ParseReference(raw string) (Reference, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Reference{}, fmt.Errorf("aikit model reference is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return Reference{}, fmt.Errorf("aikit model reference %q must not contain whitespace", raw)
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"):
		return parseGGUFURL(raw, raw)
	case strings.HasPrefix(lower, "hf.co/") || strings.HasPrefix(lower, "huggingface.co/"):
		return parseHuggingFaceShorthand(raw)
	}
	image, selector, hasSelector := strings.Cut(raw, "#")
	if hasSelector && !selectorPattern.MatchString(selector) {
		return Reference{}, fmt.Errorf("aikit model %q has an invalid #model selector", raw)
	}
	var ref Reference
	if registryQualified(image) {
		ref = Reference{Raw: raw, Kind: RefImage, Image: image}
	} else {
		parsed, err := parsePremade(image)
		if err != nil {
			return Reference{}, err
		}
		ref = parsed
		ref.Raw = raw
	}
	ref.Selector = selector
	return ref, nil
}

func parsePremade(raw string) (Reference, error) {
	name, tag, found := strings.Cut(raw, ":")
	if !found || tag == "" {
		return Reference{}, fmt.Errorf("aikit model %q needs a tag, for example aikit:qwen3.8:27b", raw)
	}
	if strings.Contains(tag, ":") || strings.Contains(tag, "@") {
		return Reference{}, fmt.Errorf("aikit model %q has an invalid tag", raw)
	}
	if !premadeNamePattern.MatchString(name) || !premadeTagPattern.MatchString(tag) {
		return Reference{}, fmt.Errorf("aikit model %q is not a valid pre-made model name", raw)
	}
	return Reference{
		Raw:     raw,
		Kind:    RefImage,
		Image:   PremadeRegistry + "/" + name + ":" + tag,
		Premade: true,
	}, nil
}

// registryQualified reports whether the first path component is a registry
// host, following the Docker reference convention.
func registryQualified(raw string) bool {
	first, rest, found := strings.Cut(raw, "/")
	if !found || rest == "" {
		return false
	}
	return first == "localhost" || strings.ContainsAny(first, ".:")
}

func parseHuggingFaceShorthand(raw string) (Reference, error) {
	_, remainder, _ := strings.Cut(raw, "/")
	if strings.HasSuffix(strings.ToLower(remainder), ".gguf") {
		parts := strings.Split(remainder, "/")
		if len(parts) < 3 {
			return Reference{}, fmt.Errorf("aikit model %q must name org/repo/file.gguf", raw)
		}
		for _, part := range parts {
			if !hfSegmentPattern.MatchString(part) {
				return Reference{}, fmt.Errorf("aikit model %q has an invalid path segment %q", raw, part)
			}
		}
		fileURL := "https://huggingface.co/" + parts[0] + "/" + parts[1] + "/resolve/main/" + strings.Join(parts[2:], "/")
		return parseGGUFURL(raw, fileURL)
	}

	repository, revision, hasRevision := strings.Cut(remainder, "@")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !hfSegmentPattern.MatchString(parts[0]) || !hfSegmentPattern.MatchString(parts[1]) {
		return Reference{}, fmt.Errorf("aikit model %q must be hf.co/org/repo@<commit> or hf.co/org/repo/file.gguf", raw)
	}
	if !hasRevision || !hfCommitPattern.MatchString(revision) {
		return Reference{}, fmt.Errorf("aikit model %q must pin a full 40-character commit, for example hf.co/%s@<commit>", raw, repository)
	}
	return Reference{
		Raw:        raw,
		Kind:       RefRunnerRepo,
		Source:     repository + "@" + revision,
		ModelName:  parts[1],
		Repository: repository,
		Revision:   revision,
	}, nil
}

func parseGGUFURL(raw, fileURL string) (Reference, error) {
	parsed, err := url.Parse(fileURL)
	if err != nil || parsed.Host == "" {
		return Reference{}, fmt.Errorf("aikit model %q is not a valid URL", raw)
	}
	secure := parsed.Scheme == "https" || parsed.Scheme == "http" && loopbackHost(parsed.Hostname())
	if !secure {
		return Reference{}, fmt.Errorf("aikit model %q must use https (plain http is allowed only for localhost)", raw)
	}
	if parsed.User != nil {
		return Reference{}, fmt.Errorf("aikit model %q must not embed credentials; set HF_TOKEN instead", raw)
	}
	// The runner receives the URL as a container argument, which is visible
	// in process listings and container metadata, so signed URLs cannot be
	// passed safely.
	if parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(fileURL, "#") {
		return Reference{}, fmt.Errorf("aikit model %q must not include a query string or fragment; the runner would expose it on the container command line", redactURL(raw))
	}
	filename := path.Base(parsed.Path)
	if !ggufFilePattern.MatchString(filename) {
		return Reference{}, fmt.Errorf("aikit model %q must point to a .gguf file with a safe filename", raw)
	}
	return Reference{
		Raw:       raw,
		Kind:      RefRunnerGGUF,
		Source:    fileURL,
		ModelName: strings.TrimSuffix(filename, ".gguf"),
	}, nil
}

func loopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// DefaultBackend returns the backend a reference uses when none is requested.
func (r Reference) DefaultBackend() string {
	if r.Kind == RefRunnerRepo {
		return BackendVLLMCPP
	}
	return BackendLlamaCPP
}

// Redacted returns the reference with any URL query or fragment removed, since
// signed download URLs carry credentials there.
func (r Reference) Redacted() string {
	if r.Kind == RefRunnerGGUF {
		return redactURL(r.Raw)
	}
	return r.Raw
}

// UsesHuggingFace reports whether the model is downloaded from Hugging Face,
// the only host HF_TOKEN may be sent to.
func (r Reference) UsesHuggingFace() bool {
	switch r.Kind {
	case RefRunnerRepo:
		return true
	case RefRunnerGGUF:
		parsed, err := url.Parse(r.Source)
		if err != nil {
			return false
		}
		host := strings.ToLower(parsed.Hostname())
		return host == "huggingface.co" || host == "hf.co" || strings.HasSuffix(host, ".huggingface.co")
	default:
		return false
	}
}

// IsRunner reports whether the reference downloads its model at startup.
func (r Reference) IsRunner() bool {
	return r.Kind == RefRunnerGGUF || r.Kind == RefRunnerRepo
}

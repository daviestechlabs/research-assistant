package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const servingIdentityContract = "dtl-vllm-serving-identity-v1"
const servingIdentityScope = "verified-read-only-loader-inputs-and-initialized-engine"
const expectedIdentityHeader = "X-DTL-Expected-Model-Identity"
const responseIdentityHeader = "X-DTL-Model-Identity"
const identityAliasHeader = "X-DTL-Model-Alias"

// These fields contain only the producer's public receipt. No endpoint,
// artifact path, credential, process ID, or environment enters a saved result.
type ServingModelIdentity struct {
	Contract        string   `json:"contract"`
	ArtifactsSHA    string   `json:"artifacts_sha256"`
	Revision        string   `json:"revision"`
	ProducerSHA     string   `json:"producer_sha256"`
	LaunchEpoch     string   `json:"launch_epoch"`
	ServingEpoch    string   `json:"serving_epoch"`
	WorkerEpochSHA  string   `json:"worker_epoch_sha256"`
	EngineConfigSHA string   `json:"engine_config_sha256"`
	VLLMVersion     string   `json:"vllm_version"`
	Aliases         []string `json:"aliases"`
	EvidenceScope   string   `json:"evidence_scope"`
	IdentitySHA     string   `json:"identity_sha256"`
}

var identityHex = regexp.MustCompile(`^[0-9a-f]{64}$`)
var identityRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var identityEpoch = regexp.MustCompile(`^[0-9a-f]{32}$`)
var identityAlias = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,160}$`)
var identityVersion = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}[A-Za-z0-9_.+-]{0,60}$`)

// Go sorts map keys. Every accepted value is ASCII and these names have no
// HTML characters, so this matches the native producer's canonical JSON.
func servingIdentityDigest(m ServingModelIdentity) string {
	b, _ := json.Marshal(map[string]any{
		"contract": m.Contract, "artifacts_sha256": m.ArtifactsSHA, "revision": m.Revision,
		"producer_sha256": m.ProducerSHA, "launch_epoch": m.LaunchEpoch, "serving_epoch": m.ServingEpoch,
		"worker_epoch_sha256": m.WorkerEpochSHA, "engine_config_sha256": m.EngineConfigSHA,
		"vllm_version": m.VLLMVersion, "aliases": m.Aliases, "evidence_scope": m.EvidenceScope,
	})
	return textSHA(b)
}

func (m ServingModelIdentity) valid() bool {
	if m.Contract != servingIdentityContract || m.EvidenceScope != servingIdentityScope ||
		!identityRevision.MatchString(m.Revision) || !identityEpoch.MatchString(m.LaunchEpoch) ||
		!identityEpoch.MatchString(m.ServingEpoch) || !identityVersion.MatchString(m.VLLMVersion) ||
		len(m.Aliases) < 1 || len(m.Aliases) > 16 {
		return false
	}
	for _, sha := range []string{m.ArtifactsSHA, m.ProducerSHA, m.WorkerEpochSHA, m.EngineConfigSHA, m.IdentitySHA} {
		if !identityHex.MatchString(sha) {
			return false
		}
	}
	seen := map[string]bool{}
	for _, name := range m.Aliases {
		if !identityAlias.MatchString(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return servingIdentityDigest(m) == m.IdentitySHA
}

// A duplicate JSON key is rejected before decoding the typed receipt.
func decodeServingIdentity(data []byte) (ServingModelIdentity, error) {
	var m ServingModelIdentity
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return m, errors.New("invalid serving identity receipt")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return m, errors.New("invalid serving identity receipt")
		}
		seen[name] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return m, errors.New("invalid serving identity receipt")
		}
	}
	if _, err = d.Token(); err != nil || len(seen) != 12 {
		return m, errors.New("invalid serving identity receipt")
	}
	if _, err = d.Token(); err != io.EOF {
		return m, errors.New("invalid serving identity receipt")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil || !m.valid() {
		return ServingModelIdentity{}, errors.New("invalid serving identity receipt")
	}
	return m, nil
}

func (i *inference) validateIdentityConfiguration() error {
	if i.identityURL == "" && i.artifactsSHA == "" && i.producerSHA == "" {
		return nil
	}
	chat, err := url.Parse(i.answerURL)
	identity, other := url.Parse(i.identityURL)
	if i.limitOwner == "litellm" {
		gateway := err == nil && approvedGatewayOrigin(chat)
		if !gateway || (chat.Scheme != "http" && chat.Scheme != "https") || chat.Path != "/v1/chat/completions" || i.key == "" || (i.model != "fast-local" && i.model != "smart-local") {
			return errors.New("gateway identity requires the private local research route and key")
		}
	}
	if err != nil || other != nil || !identityHex.MatchString(i.artifactsSHA) || !identityHex.MatchString(i.producerSHA) ||
		chat.Host == "" || (chat.Scheme != "http" && chat.Scheme != "https") ||
		identity.Scheme != chat.Scheme || identity.Host != chat.Host ||
		identity.User != nil || identity.RawQuery != "" || identity.Fragment != "" || identity.RawPath != "" || identity.Opaque != "" ||
		chat.User != nil || chat.RawQuery != "" || chat.Fragment != "" || chat.RawPath != "" || chat.Opaque != "" ||
		path.Clean(chat.Path) != chat.Path || !strings.HasSuffix(chat.Path, "/v1/chat/completions") ||
		identity.Path != strings.TrimSuffix(chat.Path, "chat/completions")+"dtl/model-identity" || i.model == "" {
		return errors.New("model identity requires its chat origin and reviewed artifact and producer checksums")
	}
	return nil
}

func (i *inference) checkServingIdentity(m ServingModelIdentity) error {
	if !m.valid() || m.ArtifactsSHA != i.artifactsSHA || m.ProducerSHA != i.producerSHA || !slices.Contains(m.Aliases, i.model) {
		return errors.New("serving model does not match the reviewed research identity")
	}
	return nil
}

func (i *inference) servingIdentity(ctx context.Context) (ServingModelIdentity, error) {
	var empty ServingModelIdentity
	if err := i.validateIdentityConfiguration(); err != nil || i.identityURL == "" {
		if err == nil {
			err = errors.New("verified serving identity is not configured")
		}
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, i.identityURL, nil)
	if err != nil {
		return empty, errors.New("invalid model identity request")
	}
	if i.key != "" {
		r.Header.Set("Authorization", "Bearer "+i.key)
	}
	if i.limitOwner == "litellm" {
		r.Header.Set(identityAliasHeader, i.model)
	}
	response, err := i.client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, errors.New("verified serving identity is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return empty, errors.New("verified serving identity is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return empty, errors.New("invalid serving identity receipt")
	}
	m, err := decodeServingIdentity(data)
	if err == nil {
		err = i.checkServingIdentity(m)
	}
	return m, err
}

type modelBindingKey struct{}
type modelBinding struct {
	Identity ServingModelIdentity
	URL      string
	Model    string
}

func (i *inference) contextWithModel(ctx context.Context, m ServingModelIdentity) context.Context {
	m.Aliases = slices.Clone(m.Aliases)
	return context.WithValue(ctx, modelBindingKey{}, modelBinding{m, i.answerURL, i.model})
}

func (i *inference) bindModel(ctx context.Context) (context.Context, error) {
	if err := i.validateIdentityConfiguration(); err != nil {
		return ctx, err
	}
	if binding, ok := ctx.Value(modelBindingKey{}).(modelBinding); ok {
		if i.identityURL == "" || binding.URL != i.answerURL || binding.Model != i.model {
			return ctx, errors.New("research model configuration changed during the request")
		}
		return ctx, i.checkServingIdentity(binding.Identity)
	}
	if i.identityURL == "" {
		return ctx, nil
	}
	m, err := i.servingIdentity(ctx)
	if err != nil {
		return ctx, err
	}
	return i.contextWithModel(ctx, m), nil
}

func modelIdentityFromContext(ctx context.Context) *ServingModelIdentity {
	binding, ok := ctx.Value(modelBindingKey{}).(modelBinding)
	if !ok {
		return nil
	}
	m := binding.Identity
	m.Aliases = slices.Clone(m.Aliases)
	return &m
}

func (i *inference) verifyBoundModel(ctx context.Context) error {
	if err := i.validateIdentityConfiguration(); err != nil {
		return err
	}
	bound := modelIdentityFromContext(ctx)
	if bound == nil {
		if i.identityURL != "" {
			return errors.New("research job has no verified model binding; submit a new job")
		}
		return nil
	}
	if _, err := i.bindModel(ctx); err != nil {
		return err
	}
	current, err := i.servingIdentity(ctx)
	if err != nil {
		return err
	}
	if current.IdentitySHA != bound.IdentitySHA {
		return errors.New("serving model identity changed; submit a new research job")
	}
	return nil
}

func validateResultModel(data string, expected *ServingModelIdentity) error {
	var result struct {
		Identity *ServingModelIdentity `json:"model_identity"`
	}
	if json.Unmarshal([]byte(data), &result) != nil ||
		(expected == nil && result.Identity != nil) ||
		(expected != nil && (result.Identity == nil || !result.Identity.valid() || result.Identity.IdentitySHA != expected.IdentitySHA)) {
		return errors.New("saved result does not match its verified model binding")
	}
	return nil
}

// Operator diagnosis reads only native metadata. It opens no catalog, usage
// ledger, research desk, or PDF and sends no inference request.
func configuredServingIdentity(ctx context.Context) (ServingModelIdentity, error) {
	i := &inference{
		answerURL: os.Getenv("RESEARCH_CHAT_URL"), model: os.Getenv("RESEARCH_CHAT_MODEL"), key: os.Getenv("RESEARCH_MODEL_KEY"), limitOwner: os.Getenv("RESEARCH_LIMIT_OWNER"),
		identityURL: os.Getenv("RESEARCH_MODEL_IDENTITY_URL"), artifactsSHA: os.Getenv("RESEARCH_MODEL_ARTIFACTS_SHA256"), producerSHA: os.Getenv("RESEARCH_MODEL_PRODUCER_SHA256"),
		client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("model identity redirects are disabled") }},
	}
	return i.servingIdentity(ctx)
}

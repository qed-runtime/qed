package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/internal/agentconfig"
	"github.com/qed-runtime/qed/internal/chatauth"
	"github.com/qed-runtime/qed/internal/tuiapp"
	"github.com/qed-runtime/qed/provider/echo"
)

func TestMain(testingMain *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == selfexec.ChildArgument {
		os.Exit(runExtension(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(testingMain.Run())
}

type fakeChatAuthService struct {
	loginBrowser func(context.Context, string, chatauth.BrowserLoginOptions) (chatauth.ProfileInfo, error)
	loginDevice  func(context.Context, string, chatauth.DeviceLoginOptions) (chatauth.ProfileInfo, error)
	profileList  []chatauth.ProfileInfo
	profilesErr  error
	logout       func(context.Context, string) (chatauth.LogoutResult, error)
}

func (service *fakeChatAuthService) LoginBrowser(
	ctx context.Context,
	profileID string,
	options chatauth.BrowserLoginOptions,
) (chatauth.ProfileInfo, error) {
	if service.loginBrowser == nil {
		return chatauth.ProfileInfo{}, errors.New("unexpected browser login")
	}
	return service.loginBrowser(ctx, profileID, options)
}

func (service *fakeChatAuthService) LoginDevice(
	ctx context.Context,
	profileID string,
	options chatauth.DeviceLoginOptions,
) (chatauth.ProfileInfo, error) {
	if service.loginDevice == nil {
		return chatauth.ProfileInfo{}, errors.New("unexpected device login")
	}
	return service.loginDevice(ctx, profileID, options)
}

func (service *fakeChatAuthService) Profiles(context.Context) ([]chatauth.ProfileInfo, error) {
	return append([]chatauth.ProfileInfo(nil), service.profileList...), service.profilesErr
}

func (service *fakeChatAuthService) Logout(ctx context.Context, profileID string) (chatauth.LogoutResult, error) {
	if service.logout == nil {
		return chatauth.LogoutResult{}, errors.New("unexpected logout")
	}
	return service.logout(ctx, profileID)
}

func TestRunTextOutput(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"run", "hello"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); got != "hello\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestExtensionGenerateWritesAndChecksCatalog(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	lockPath := filepath.Join(directory, "extensions.lock")
	outputPath := filepath.Join(directory, "registry_gen.go")
	document := `{
		"version":1,
		"extensions":[{
			"go_package":"github.com/qed-runtime/qed/extensions/git",
			"manifest":{
				"id":"qed.git",
				"version":"0.1.0",
				"protocol_version":1,
				"capabilities":["git.read"]
			}
		}]
	}`
	if err := os.WriteFile(lockPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	var generateOutput bytes.Buffer
	var generateError bytes.Buffer
	exitCode := run(context.Background(), []string{
		"extension", "generate", "--lock", lockPath, "--output", outputPath,
		"--package", "customregistry", "--variable", "Linked",
	}, &generateOutput, &generateError)
	if exitCode != 0 || !strings.Contains(generateOutput.String(), "Generated Extension catalog") || generateError.Len() != 0 {
		t.Fatalf("generate exit/stdout/stderr = %d/%q/%q", exitCode, generateOutput.String(), generateError.String())
	}
	generated, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), `package customregistry`) ||
		!strings.Contains(string(generated), `var Linked = selfexec.MustNewCatalog`) ||
		!strings.Contains(string(generated), `ServerOptions: extension0.ServerOptions`) {
		t.Fatalf("generated catalog = %s", generated)
	}

	var checkOutput bytes.Buffer
	var checkError bytes.Buffer
	exitCode = run(context.Background(), []string{
		"extension", "generate", "--lock", lockPath, "--output", outputPath,
		"--package", "customregistry", "--variable", "Linked", "--check",
	}, &checkOutput, &checkError)
	if exitCode != 0 || !strings.Contains(checkOutput.String(), "is current") || checkError.Len() != 0 {
		t.Fatalf("check exit/stdout/stderr = %d/%q/%q", exitCode, checkOutput.String(), checkError.String())
	}
	if err := os.WriteFile(outputPath, append(generated, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	checkOutput.Reset()
	checkError.Reset()
	exitCode = run(context.Background(), []string{
		"extension", "generate", "--lock", lockPath, "--output", outputPath,
		"--package", "customregistry", "--variable", "Linked", "--check",
	}, &checkOutput, &checkError)
	if exitCode == 0 || !strings.Contains(checkError.String(), "is stale") {
		t.Fatalf("stale check exit/stdout/stderr = %d/%q/%q", exitCode, checkOutput.String(), checkError.String())
	}
}

func TestExtensionScaffoldCreatesGoReferenceAndRejectsExistingDestination(t *testing.T) {
	t.Parallel()

	moduleRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(moduleRoot, "go.mod"), []byte("module example.com/scaffold\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(moduleRoot, "sample")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"extension", "scaffold", directory,
		"--id", "example.sample",
		"--extension-version", "1.2.3",
	}, &stdout, &stderr)
	if exitCode != 0 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), "Created Go Extension scaffold") ||
		!strings.Contains(stdout.String(), "example.com/scaffold/sample/extension") {
		t.Fatalf("scaffold exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	for _, relative := range []string{
		".gitignore",
		"README.md",
		"extension/extension.go",
		"main.go",
		"main_test.go",
		"qed-extension.json",
	} {
		if _, err := os.Stat(filepath.Join(directory, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("generated file %s: %v", relative, err)
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(directory, "qed-extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var generatedManifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifestData, &generatedManifest); err != nil {
		t.Fatal(err)
	}
	if generatedManifest.ID != "example.sample" || generatedManifest.Version != "1.2.3" {
		t.Fatalf("generated manifest = %#v", generatedManifest)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run(context.Background(), []string{
		"extension", "scaffold", directory,
		"--id", "example.changed",
	}, &stdout, &stderr)
	if exitCode == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "destination already exists") {
		t.Fatalf("second scaffold exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestExtensionScaffoldHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"extension", "scaffold", "--help"}, &stdout, &stderr)
	if exitCode != 0 || stderr.Len() != 0 {
		t.Fatalf("help exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"qed extension scaffold", "DIRECTORY", "--id", "--extension-version"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("help output does not contain %q:\n%s", expected, stdout.String())
		}
	}
}

func TestAuthLoginUsesNamedBrowserProfile(t *testing.T) {
	t.Parallel()

	fake := &fakeChatAuthService{}
	fake.loginBrowser = func(_ context.Context, profileID string, options chatauth.BrowserLoginOptions) (chatauth.ProfileInfo, error) {
		if profileID != "personal" {
			t.Errorf("profile ID = %q", profileID)
		}
		options.PresentURL("https://auth.openai.com/oauth/authorize?state=test")
		return chatauth.ProfileInfo{ID: profileID, Email: "user@example.com"}, nil
	}
	dependencies := defaultCommandDependencies()
	dependencies.newChatAuthService = func() (chatAuthService, error) { return fake, nil }
	var opened string
	dependencies.openURL = func(_ context.Context, value string) error {
		opened = value
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"auth", "login", "--auth-profile", "personal"},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if opened == "" || !strings.Contains(stdout.String(), opened) ||
		!strings.Contains(stdout.String(), `Logged in ChatGPT auth profile "personal" as user@example.com`) {
		t.Fatalf("opened/stdout = %q/%q", opened, stdout.String())
	}
}

func TestAuthLoginDeviceCodeDoesNotOpenBrowser(t *testing.T) {
	t.Parallel()

	fake := &fakeChatAuthService{}
	fake.loginDevice = func(_ context.Context, profileID string, options chatauth.DeviceLoginOptions) (chatauth.ProfileInfo, error) {
		options.PresentCode(chatauth.DeviceCode{VerificationURL: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH"})
		return chatauth.ProfileInfo{ID: profileID}, nil
	}
	dependencies := defaultCommandDependencies()
	dependencies.newChatAuthService = func() (chatAuthService, error) { return fake, nil }
	dependencies.openURL = func(context.Context, string) error {
		t.Fatal("browser was opened for device login")
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"auth", "login", "--auth-profile", "server", "--device-code"},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 || !strings.Contains(stdout.String(), "ABCD-EFGH") || !strings.Contains(stdout.String(), `"server"`) {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestAuthStatusAndLogoutDoNotExposeCredentials(t *testing.T) {
	t.Parallel()

	fake := &fakeChatAuthService{}
	fake.profileList = []chatauth.ProfileInfo{{
		ID: "personal", Email: "user@example.com", Plan: "plus",
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	fake.logout = func(_ context.Context, profileID string) (chatauth.LogoutResult, error) {
		if profileID != "personal" {
			t.Errorf("profile ID = %q", profileID)
		}
		return chatauth.LogoutResult{Removed: true, RevocationError: errors.New("offline")}, nil
	}
	dependencies := defaultCommandDependencies()
	dependencies.newChatAuthService = func() (chatAuthService, error) { return fake, nil }
	var statusOutput bytes.Buffer
	var statusError bytes.Buffer
	if exitCode := runWithDependencies(
		context.Background(),
		[]string{"auth", "status", "--auth-profile", "personal"},
		&statusOutput,
		&statusError,
		dependencies,
	); exitCode != 0 {
		t.Fatalf("status exit/stderr = %d/%q", exitCode, statusError.String())
	}
	if !strings.Contains(statusOutput.String(), "personal\tvalid") || strings.Contains(statusOutput.String(), "access-token") {
		t.Fatalf("status output = %q", statusOutput.String())
	}
	var logoutOutput bytes.Buffer
	var logoutError bytes.Buffer
	if exitCode := runWithDependencies(
		context.Background(),
		[]string{"auth", "logout", "--auth-profile", "personal"},
		&logoutOutput,
		&logoutError,
		dependencies,
	); exitCode != 0 {
		t.Fatalf("logout exit/stderr = %d/%q", exitCode, logoutError.String())
	}
	if !strings.Contains(logoutOutput.String(), `"personal"`) || !strings.Contains(logoutError.String(), "local credentials were removed") {
		t.Fatalf("logout output/error = %q/%q", logoutOutput.String(), logoutError.String())
	}
}

func TestRunVerboseWritesStructuredDiagnosticsWithoutPromptContent(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"run", "-v", "do-not-log-prompt"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"msg":"run.execution.started"`) ||
		!strings.Contains(stderr.String(), `"msg":"provider.stream.completed"`) {
		t.Fatalf("stderr does not contain debug diagnostics: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "do-not-log-prompt") {
		t.Fatalf("stderr contains prompt content: %s", stderr.String())
	}
}

func TestEvidenceInspectReadsEvidenceBundle(t *testing.T) {
	t.Parallel()

	storeRoot := filepath.Join(t.TempDir(), "evidence")
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := evidence.NewBundle(agent.RunResult{
		RunID: "run_inspect_test", Status: agent.RunStatusCompleted,
	}, evidence.BundleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"evidence", "inspect", "run_inspect_test", "--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Run: run_inspect_test") || !strings.Contains(stdout.String(), "Status: completed") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEvidenceFetchReadsContentAddressedObject(t *testing.T) {
	t.Parallel()

	storeRoot := filepath.Join(t.TempDir(), "evidence")
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.PutObject(context.Background(), "text/plain", []byte("exact object content"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"evidence", "fetch", reference.Digest, "--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode != 0 || stdout.String() != "exact object content" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestEvidenceFetchResolvesAndAuditsScopedObjectFromRunBundle(t *testing.T) {
	t.Parallel()

	storeRoot := filepath.Join(t.TempDir(), "evidence")
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	access := agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: "tenant", SessionID: "session", ProfileID: "profile",
		},
		PrincipalID:  "runtime",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
	reference, err := store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access:               access,
		MediaType:            "text/plain",
		Content:              []byte("scoped object content"),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := evidence.NewBundle(agent.RunResult{
		RunID: "run_scoped_fetch", Status: agent.RunStatusCompleted,
	}, evidence.BundleOptions{Events: []agent.Event{{
		Sequence: 1, Type: agent.EventContextCompacted, RunID: "run_scoped_fetch",
		ContextCompaction: &agent.ContextCompactionReport{
			Applied: true, Reason: "externalize_evidence", Externalized: []agent.EvidenceObjectRef{reference},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"evidence", "fetch", reference.Digest,
		"--run-id", "run_scoped_fetch",
		"--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode != 0 || stdout.String() != "scoped object content" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	records, err := store.EvidenceObjectAccessRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Operation != agent.EvidenceObjectAccessAdminGet ||
		records[1].Outcome != agent.EvidenceObjectAccessAllowed {
		t.Fatalf("access records = %#v", records)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run(context.Background(), []string{
		"evidence", "fetch", reference.Digest, "--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode == 0 || !strings.Contains(stderr.String(), "use --run-id") {
		t.Fatalf("unscoped exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestEvidenceReferenceFromBundleRejectsInvalidMatchingReference(t *testing.T) {
	t.Parallel()

	access := agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: "tenant", SessionID: "session", ProfileID: "profile",
		},
		PrincipalID:  "runtime",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
	reference, err := agent.BindEvidenceObjectReference(agent.EvidenceObjectRef{
		Digest: "sha256:" + strings.Repeat("a", 64), Bytes: 7, MediaType: "text/plain",
	}, access, []string{agent.EvidenceReadCapability}, agent.EvidenceSensitivityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	reference.Scope.BindingDigest = "invalid"
	bundle := evidence.Bundle{Events: []agent.Event{{
		ContextCompaction: &agent.ContextCompactionReport{
			Externalized: []agent.EvidenceObjectRef{reference},
		},
	}}}
	if _, err := evidenceReferenceFromBundle(bundle, reference.Digest); err == nil ||
		!strings.Contains(err.Error(), "validate Evidence Object reference") {
		t.Fatalf("evidenceReferenceFromBundle() error = %v", err)
	}
}

func TestCacheStatusReadsPlanUsageAndCost(t *testing.T) {
	t.Parallel()

	storeRoot := filepath.Join(t.TempDir(), "evidence")
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	pricing := &agent.CachePricing{
		Currency:                      "USD",
		UncachedInputMicrosPerMillion: 2_000_000,
		CacheReadMicrosPerMillion:     200_000,
		CacheWriteMicrosPerMillion:    2_500_000,
		OutputMicrosPerMillion:        10_000_000,
	}
	forecast, err := agent.ForecastCacheCost(*pricing, 2000, 3)
	if err != nil {
		t.Fatal(err)
	}
	manifest := agent.PrefixManifest{
		Version:     1,
		Provider:    "openai/responses:primary",
		Model:       "gpt-5.6-luna",
		CacheFamily: "cache_" + strings.Repeat("a", 64),
		Epoch:       "epoch-1",
		Segments: []agent.SegmentFingerprint{{
			ID:          "instructions",
			Kind:        agent.SegmentKindInstructions,
			Version:     "1",
			ContentHash: "sha256:" + strings.Repeat("b", 64),
			Bytes:       8000,
			Stability:   agent.StabilityProject,
		}},
	}
	plan := agent.CachePlan{
		Version:            1,
		FamilyID:           manifest.CacheFamily,
		Mode:               agent.CacheModeExplicit,
		TTL:                agent.CacheTTLThirtyMinutes,
		ExpectedReuse:      3,
		InputTokenEstimate: 2000,
		TokenEstimateKind:  "canonical_bytes_div_4",
		Pricing:            pricing,
		Forecast:           &forecast,
		Breakpoints: []agent.CacheBreakpoint{{
			AfterSegmentID:      "message/0000000000",
			MessageIndex:        0,
			Write:               true,
			PrefixTokenEstimate: 2000,
		}},
	}
	usage := agent.Usage{
		InputTokens:               100,
		OutputTokens:              10,
		TotalTokens:               110,
		InputTokenDetailsReported: true,
		UncachedInputTokens:       20,
		CacheReadInputTokens:      70,
		CacheWriteInputTokens:     10,
	}
	bundle, err := evidence.NewBundle(agent.RunResult{
		RunID:  "run_cache_status",
		Status: agent.RunStatusCompleted,
		Messages: []agent.Message{{
			Role:  agent.RoleAssistant,
			Model: "gpt-5.6-luna",
		}},
		Usage: usage,
	}, evidence.BundleOptions{Events: []agent.Event{
		{Type: agent.EventRunStarted, RunID: "run_cache_status", Sequence: 1},
		{
			Type: agent.EventModelRequest, RunID: "run_cache_status", Sequence: 2,
			ProviderCall: 1, ProviderAttempt: 1,
			PrefixManifest: &manifest, CachePlan: &plan,
		},
		{
			Type: agent.EventMessageCompleted, RunID: "run_cache_status", Sequence: 3,
			Message: &agent.Message{Role: agent.RoleAssistant, Usage: &usage},
		},
		{Type: agent.EventRunCompleted, RunID: "run_cache_status", Sequence: 4},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"cache", "status", "run_cache_status", "--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit/stderr = %d/%q", exitCode, stderr.String())
	}
	for _, want := range []string{
		"Provider: openai/responses:primary",
		"Mode: explicit",
		"cache_read_ratio=70.00%",
		"Forecast: currency=USD",
		"Estimated actual cost: currency=USD",
		"Input estimate comparison: estimated=2000 actual=100 difference=-1900 kind=canonical_bytes_div_4",
		"First divergence: none",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("cache status output %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestExecJSONShortcutOutput(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"exec", "--json", "hello"},
		&stdout,
		&stderr,
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}

	var eventTypes []agent.EventType
	var prefixManifest *agent.PrefixManifest
	scanner := bufio.NewScanner(strings.NewReader(stdout.String()))
	for scanner.Scan() {
		var event agent.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode event %q: %v", scanner.Text(), err)
		}
		eventTypes = append(eventTypes, event.Type)
		if event.Type == agent.EventModelRequest {
			prefixManifest = event.PrefixManifest
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}

	want := []agent.EventType{
		agent.EventRunStarted,
		agent.EventUserMessageAdded,
		agent.EventModelRequest,
		agent.EventMessageStarted,
		agent.EventMessageDelta,
		agent.EventMessageCompleted,
		agent.EventRunCompleted,
	}
	if len(eventTypes) != len(want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
	for index := range want {
		if eventTypes[index] != want[index] {
			t.Errorf("eventTypes[%d] = %q, want %q", index, eventTypes[index], want[index])
		}
	}
	if prefixManifest == nil || prefixManifest.Provider != "echo" || prefixManifest.Epoch == "" || len(prefixManifest.Segments) != 3 {
		t.Fatalf("Prefix Manifest = %#v", prefixManifest)
	}
}

func TestRunRequiresPrompt(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"run"}, &stdout, &stderr)

	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing-required") ||
		!strings.Contains(stderr.String(), "prompt") {
		t.Errorf("stderr = %q, want structured prompt error", stderr.String())
	}
}

func TestRunRejectsBlankPrompt(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"run", "   "}, &stdout, &stderr)

	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "error[validation]: prompt must not be empty") {
		t.Errorf("stderr = %q, want blank prompt diagnostic", stderr.String())
	}
}

func TestRunHelpUsesNagiRenderer(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"run", "--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	for _, expected := range []string{
		"Usage:",
		"qed run",
		"<PROMPT>",
		"--output",
		"--json",
		"--provider",
		"--model",
		"--base-url",
		"--auth-profile",
		"--system",
		"--config",
		"--agent",
		"--workspace",
		"--cd",
		"--approval",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("stdout = %q, want %q", stdout.String(), expected)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestInteractiveCommandsInvokeTUIWithOptionalPrompt(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		arguments  []string
		wantPrompt string
	}{
		{name: "root idle", arguments: nil, wantPrompt: ""},
		{name: "root prompted", arguments: []string{"hello"}, wantPrompt: "hello"},
		{name: "explicit idle", arguments: []string{"tui"}, wantPrompt: ""},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var received agent.RunRequest
			var receivedPrompt string
			dependencies := defaultCommandDependencies()
			dependencies.runTUI = func(
				_ context.Context,
				_ tuiapp.StartFunc,
				request agent.RunRequest,
				prompt string,
				_ tuiapp.ChatOptions,
			) (tuiapp.Outcome, error) {
				received = request
				receivedPrompt = prompt
				return tuiapp.Outcome{}, nil
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDependencies(
				context.Background(),
				test.arguments,
				&stdout,
				&stderr,
				dependencies,
			)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if receivedPrompt != test.wantPrompt {
				t.Fatalf("prompt = %q, want %q", receivedPrompt, test.wantPrompt)
			}
			if test.wantPrompt == "" && len(received.Input) != 0 {
				t.Fatalf("idle Run input = %#v, want empty", received.Input)
			}
			if test.wantPrompt != "" && (len(received.Input) != 1 || received.Input[0].Text != test.wantPrompt) {
				t.Fatalf("prompted Run input = %#v", received.Input)
			}
		})
	}
}

func TestInteractiveCommandsRejectExplicitBlankPrompt(t *testing.T) {
	t.Parallel()

	for _, arguments := range [][]string{{"   "}, {"tui", "   "}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(context.Background(), arguments, &stdout, &stderr)
		if exitCode != 2 || !strings.Contains(stderr.String(), "prompt must not be empty") {
			t.Errorf("arguments/exit/stderr = %q/%d/%q", arguments, exitCode, stderr.String())
		}
	}
}

func TestTUICommandInvokesRunner(t *testing.T) {
	t.Parallel()

	var receivedPrompt string
	var receivedAgentID string
	var receivedInstructions string
	dependencies := defaultCommandDependencies()
	dependencies.runTUI = func(
		_ context.Context,
		_ tuiapp.StartFunc,
		request agent.RunRequest,
		prompt string,
		_ tuiapp.ChatOptions,
	) (tuiapp.Outcome, error) {
		receivedAgentID = request.AgentID
		receivedInstructions = request.Instructions
		receivedPrompt = prompt
		return tuiapp.Outcome{}, nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"tui", "--system", "be concise", "hello"},
		&stdout,
		&stderr,
		dependencies,
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if receivedPrompt != "hello" {
		t.Errorf("prompt = %q, want hello", receivedPrompt)
	}
	if receivedAgentID != providerEcho || receivedInstructions != "be concise" {
		t.Errorf("agent ID/instructions = %q/%q", receivedAgentID, receivedInstructions)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q, want empty", stdout.String(), stderr.String())
	}
}

func TestTUICommandLoadsConfiguredAgentAndSession(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(configPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"session":{"store":"memory"},
		"evidence":{"store":"json","path":"evidence"},
		"agents":{"main":{"provider":"local"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := defaultCommandDependencies()
	var received agent.RunRequest
	dependencies.runTUI = func(
		_ context.Context,
		start tuiapp.StartFunc,
		request agent.RunRequest,
		_ string,
		options tuiapp.ChatOptions,
	) (tuiapp.Outcome, error) {
		if start == nil {
			t.Fatal("configured TUI starter is nil")
		}
		received = request
		if options.SessionStore == nil {
			t.Fatal("configured TUI Session Store is nil")
		}
		return tuiapp.Outcome{
			Result: agent.RunResult{RunID: "tui_evidence_2", AgentID: "main", Status: agent.RunStatusCompleted},
			Events: []agent.Event{{RunID: "tui_evidence_2", AgentID: "main", Type: agent.EventRunCompleted}},
			Runs: []tuiapp.RunOutcome{
				{
					Result: agent.RunResult{RunID: "tui_evidence_1", AgentID: "main", Status: agent.RunStatusCompleted},
					Events: []agent.Event{{RunID: "tui_evidence_1", AgentID: "main", Type: agent.EventRunCompleted}},
				},
				{
					Result: agent.RunResult{RunID: "tui_evidence_2", AgentID: "main", Status: agent.RunStatusCompleted},
					Events: []agent.Event{{RunID: "tui_evidence_2", AgentID: "main", Type: agent.EventRunCompleted}},
				},
			},
		}, nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(context.Background(), []string{
		"tui", "--verbose", "--config", configPath, "--agent", "main", "--session-id", "session-tui", "hello",
	}, &stdout, &stderr, dependencies)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if received.AgentID != "main" || received.SessionID != "session-tui" || len(received.Input) != 1 || received.Input[0].Text != "hello" {
		t.Fatalf("RunRequest = %#v", received)
	}
	store, err := evidence.NewJSONStore(filepath.Join(filepath.Dir(configPath), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"tui_evidence_1", "tui_evidence_2"} {
		if _, err := store.Load(context.Background(), runID); err != nil {
			t.Fatalf("load TUI Evidence %q = %v", runID, err)
		}
	}
}

func TestRunSessionIDRequiresConfiguredStore(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(configPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"agents":{"main":{"provider":"local"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"run", "--config", configPath, "--session-id", "missing-store", "hello",
	}, &stdout, &stderr)
	if exitCode != 1 || !strings.Contains(stderr.String(), "configuration has no Session Store") {
		t.Fatalf("exit/stderr = %d/%q", exitCode, stderr.String())
	}
}

func TestRunProviderConfigurationReachesFactory(t *testing.T) {
	t.Parallel()

	var received runtimeConfig
	dependencies := defaultCommandDependencies()
	dependencies.newRuntime = func(config runtimeConfig) (*agent.Runtime, error) {
		received = config
		return agent.NewRuntime(agent.Options{Provider: echo.New()})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{
			"run",
			"--provider", providerOpenAIResponses,
			"-m", "test-model",
			"--base-url", "http://127.0.0.1:8080/v1",
			"--system", "be concise",
			"--max-output-tokens", "64",
			"hello",
		},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if received.provider != providerOpenAIResponses || received.model != "test-model" ||
		received.baseURL != "http://127.0.0.1:8080/v1" || received.instructions != "be concise" ||
		received.maxOutputTokens != 64 {
		t.Errorf("runtime config = %#v", received)
	}
}

func TestRunOpenAICodexConfigurationReachesFactory(t *testing.T) {
	t.Parallel()

	var received runtimeConfig
	dependencies := defaultCommandDependencies()
	dependencies.newRuntime = func(config runtimeConfig) (*agent.Runtime, error) {
		received = config
		return agent.NewRuntime(agent.Options{Provider: echo.New()})
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{
			"run",
			"--provider", providerOpenAICodex,
			"--model", "gpt-test",
			"--auth-profile", "personal",
			"hello",
		},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if received.provider != providerOpenAICodex || received.model != "gpt-test" || received.authProfile != "personal" ||
		received.apiKey != "" || received.baseURL != "" {
		t.Fatalf("runtime config = %#v", received)
	}
}

func TestRunOpenAICodexRequiresAuthProfileAndRejectsCustomEndpoint(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{
			name:      "missing auth profile",
			arguments: []string{"run", "--provider", providerOpenAICodex, "--model", "gpt-test", "hello"},
			want:      "--auth-profile is required",
		},
		{
			name: "custom endpoint",
			arguments: []string{
				"run", "--provider", providerOpenAICodex, "--model", "gpt-test",
				"--auth-profile", "personal", "--base-url", "https://example.invalid", "hello",
			},
			want: "--base-url is not supported",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := run(context.Background(), test.arguments, &stdout, &stderr)
			if exitCode != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("exit/stderr = %d/%q, want %q", exitCode, stderr.String(), test.want)
			}
		})
	}
}

func TestRunModelProviderRequiresModel(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--provider", providerAnthropic, "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--model is required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestCustomBaseURLUsesOnlyExplicitCustomKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "official-key")
	t.Setenv("QED_API_KEY", "custom-key")

	var received runtimeConfig
	dependencies := defaultCommandDependencies()
	dependencies.newRuntime = func(config runtimeConfig) (*agent.Runtime, error) {
		received = config
		return agent.NewRuntime(agent.Options{Provider: echo.New()})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{
			"run",
			"--provider", providerOpenAIChat,
			"--model", "test-model",
			"--base-url", "https://trusted.example/v1",
			"hello",
		},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if received.apiKey != "custom-key" {
		t.Errorf("API key = %q, want custom-key", received.apiKey)
	}
}

func TestDefaultOpenAIEndpointRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{
			"run",
			"--provider", providerOpenAIResponses,
			"--model", "test-model",
			"hello",
		},
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "OPENAI_API_KEY is required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunOpenAIResponsesAgainstHTTPServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", request.URL.Path)
		}
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "test-model" || len(body.Input) != 1 || body.Input[0].Content != "hello" {
			t.Errorf("request body = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
            "id":"resp_cli",
            "model":"test-model",
            "status":"completed",
            "output":[{
                "type":"message",
                "role":"assistant",
                "content":[{"type":"output_text","text":"from model"}]
            }],
            "usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
        }`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{
			"run",
			"--provider", providerOpenAIResponses,
			"--model", "test-model",
			"--base-url", server.URL + "/v1",
			"hello",
		},
		&stdout,
		&stderr,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if stdout.String() != "from model\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunConfiguredCodingProfileAgainstOpenAIProtocol(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
			Tools        []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		wantTools := []string{"apply_patch", "git_diff", "git_status", "read_file", "run_command", "search_text"}
		if len(body.Tools) != len(wantTools) {
			t.Errorf("Tool count = %d, want %d", len(body.Tools), len(wantTools))
		} else {
			for index, name := range wantTools {
				if body.Tools[index].Name != name {
					t.Errorf("Tool[%d] = %q, want %q", index, body.Tools[index].Name, name)
				}
			}
		}
		if !strings.Contains(body.Instructions, "coding agent operating within one bounded workspace") {
			t.Errorf("Coding instructions = %q", body.Instructions)
		}

		writer.Header().Set("Content-Type", "application/json")
		switch requests.Add(1) {
		case 1:
			_, _ = writer.Write([]byte(`{
				"id":"coding_response_1",
				"model":"coding-model",
				"status":"completed",
				"output":[{
					"type":"function_call",
					"id":"function_1",
					"call_id":"call_read",
					"name":"read_file",
					"arguments":"{\"path\":\"note.txt\"}",
					"status":"completed"
				}],
				"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}
			}`))
		case 2:
			var output string
			for _, rawItem := range body.Input {
				var item struct {
					Type   string `json:"type"`
					Output string `json:"output"`
				}
				_ = json.Unmarshal(rawItem, &item)
				if item.Type == "function_call_output" {
					output = item.Output
				}
			}
			if !strings.Contains(output, `"path":"note.txt"`) || !strings.Contains(output, `workspace data\n`) {
				t.Errorf("read_file output = %q", output)
			}
			_, _ = writer.Write([]byte(`{
				"id":"coding_response_2",
				"model":"coding-model",
				"status":"completed",
				"output":[{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"coding profile connected"}]
				}],
				"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}
			}`))
		default:
			t.Errorf("unexpected request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "note.txt"), []byte("workspace data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf(`{
		"version":1,
		"default_agent":"coding",
		"providers":{"model":{"protocol":"openai-responses","base_url":%q,"model":"coding-model"}},
		"extensions":{
			"qed.workspace":{"mode":"self-exec"},
			"qed.process":{"mode":"self-exec"},
			"qed.git":{"mode":"self-exec"}
		},
		"profiles":{"workspace":{"kind":"coding","extensions":["qed.workspace","qed.process","qed.git"],"capabilities":{"ask":["filesystem.read"]}}},
		"agents":{"coding":{"provider":"model","profile":"workspace"}}
	}`, server.URL+"/v1")
	configPath := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithInputAndDependencies(
		context.Background(),
		[]string{"run", "--verbose", "--config", configPath, "--workspace", workspaceRoot, "--approval", "prompt", "Read the note"},
		strings.NewReader("yes\n"),
		&stdout,
		&stderr,
		defaultCommandDependencies(),
	)
	if exitCode != 0 || stdout.String() != "coding profile connected\n" ||
		!strings.Contains(stderr.String(), `Approval required for Tool "read_file"`) ||
		!strings.Contains(stderr.String(), "filesystem.read") ||
		!strings.Contains(stderr.String(), "extension.initialized") ||
		!strings.Contains(stderr.String(), "extension.process.ready") ||
		!strings.Contains(stderr.String(), `"extension_id":"qed.workspace"`) ||
		!strings.Contains(stderr.String(), `"extension_id":"qed.process"`) ||
		!strings.Contains(stderr.String(), `"extension_id":"qed.git"`) ||
		strings.Contains(stderr.String(), "note.txt") {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	if requests.Load() != 2 {
		t.Fatalf("request count = %d, want 2", requests.Load())
	}
}

func TestRunConfiguredOpenAIParentWithAnthropicChild(t *testing.T) {
	t.Setenv("PRIMARY_ACCESS_TOKEN", "primary-token")
	t.Setenv("REVIEW_ACCESS_TOKEN", "review-token")

	var anthropicRequests atomic.Int32
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		anthropicRequests.Add(1)
		if request.URL.Path != "/v1/messages" {
			t.Errorf("Anthropic path = %q", request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "review-token" {
			t.Errorf("Anthropic x-api-key = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("Anthropic Authorization = %q, want empty", got)
		}
		var body struct {
			System   string `json:"system"`
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode Anthropic request: %v", err)
		}
		if body.System != "Review delegated work" || len(body.Messages) != 1 ||
			len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0].Text != "Review endpoint pairing" {
			t.Errorf("Anthropic request = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id":"child_message",
			"type":"message",
			"role":"assistant",
			"model":"review-model",
			"content":[{"type":"text","text":"review result"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":2,"output_tokens":2}
		}`))
	}))
	defer anthropicServer.Close()

	var openAIRequests atomic.Int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("OpenAI path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer primary-token" {
			t.Errorf("OpenAI Authorization = %q", got)
		}
		if got := request.Header.Get("x-api-key"); got != "" {
			t.Errorf("OpenAI x-api-key = %q, want empty", got)
		}
		var body struct {
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
			Tools        []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode OpenAI request: %v", err)
		}
		if body.Instructions != "Coordinate specialists" || len(body.Tools) != 1 || body.Tools[0].Name != "consult_review" {
			t.Errorf("OpenAI instructions/tools = %q/%#v", body.Instructions, body.Tools)
		}

		writer.Header().Set("Content-Type", "application/json")
		switch openAIRequests.Add(1) {
		case 1:
			_, _ = writer.Write([]byte(`{
				"id":"parent_response_1",
				"model":"primary-model",
				"status":"completed",
				"output":[{
					"type":"function_call",
					"id":"function_1",
					"call_id":"call_1",
					"name":"consult_review",
					"arguments":"{\"prompt\":\"Review endpoint pairing\"}",
					"status":"completed"
				}],
				"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
			}`))
		case 2:
			var functionOutput string
			for _, rawItem := range body.Input {
				var item struct {
					Type   string `json:"type"`
					Output string `json:"output"`
				}
				_ = json.Unmarshal(rawItem, &item)
				if item.Type == "function_call_output" {
					functionOutput = item.Output
				}
			}
			if !strings.Contains(functionOutput, `"agent_id":"reviewer"`) ||
				!strings.Contains(functionOutput, `"output":"review result"`) {
				t.Errorf("function output = %q", functionOutput)
			}
			_, _ = writer.Write([]byte(`{
				"id":"parent_response_2",
				"model":"primary-model",
				"status":"completed",
				"output":[{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"configured final answer"}]
				}],
				"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}
			}`))
		default:
			t.Errorf("unexpected OpenAI request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer openAIServer.Close()

	document := fmt.Sprintf(`{
		"version": 1,
		"default_agent": "coordinator",
		"providers": {
			"primary": {
				"protocol": "openai-responses",
				"base_url": %q,
				"model": "primary-model",
				"token_env": "PRIMARY_ACCESS_TOKEN"
			},
			"review": {
				"protocol": "anthropic",
				"base_url": %q,
				"model": "review-model",
				"token_env": "REVIEW_ACCESS_TOKEN"
			}
		},
		"agents": {
			"reviewer": {
				"provider": "review",
				"instructions": "Review delegated work"
			},
			"coordinator": {
				"provider": "primary",
				"instructions": "Coordinate specialists",
				"delegations": [{
					"name": "consult_review",
					"strategy": "delegate",
					"agents": ["reviewer"]
				}]
			}
		}
	}`, openAIServer.URL+"/v1", anthropicServer.URL+"/v1")
	configPath := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--config", configPath, "Evaluate endpoints"},
		&stdout,
		&stderr,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if stdout.String() != "configured final answer\n" || stderr.Len() != 0 {
		t.Errorf("stdout/stderr = %q/%q", stdout.String(), stderr.String())
	}
	if openAIRequests.Load() != 2 || anthropicRequests.Load() != 1 {
		t.Errorf("OpenAI/Anthropic requests = %d/%d", openAIRequests.Load(), anthropicRequests.Load())
	}
}

func TestRunConfigConflictsWithInlineProviderOptions(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--config", "qed.json", "--provider", "echo", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conflict") || !strings.Contains(stderr.String(), "--config") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRootTUIOptionBeforeSubcommandIsRejected(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"--config", "qed.json", "run", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "configures the root TUI") ||
		!strings.Contains(stderr.String(), `after subcommand "run"`) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunAgentOptionRequiresConfig(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--agent", "coordinator", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires") || !strings.Contains(stderr.String(), "config") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunPassesShortCDToAgentConfiguration(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(configPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"agents":{"main":{"provider":"local"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	dependencies := defaultCommandDependencies()
	load := dependencies.loadAgentConfig
	var received agentconfig.LoadOptions
	dependencies.loadAgentConfig = func(path string, options agentconfig.LoadOptions) (*agentconfig.Configuration, error) {
		received = options
		return load(path, options)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"run", "--verbose", "--config", configPath, "-C", workspaceRoot, "--approval", "prompt", "hello"},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if received.WorkspaceRoot != workspaceRoot || received.LookupEnv == nil || received.Approver == nil ||
		received.SelfExecCatalog == nil || !received.Verbose || received.DebugWriter == nil {
		t.Fatalf("LoadOptions = %#v", received)
	}
}

func TestRunApprovalOptionRequiresConfig(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--approval", "prompt", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "requires") || !strings.Contains(stderr.String(), "config") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunWorkspaceOptionRequiresConfig(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--workspace", ".", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires") || !strings.Contains(stderr.String(), "config") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsMultipleWorkspaceRoots(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"run", "--config", "qed.json", "--workspace", ".", "--cd", ".", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "option-group") || !strings.Contains(stderr.String(), "workspace-root") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestCanceledCommandUsesNagiCancellationStatus(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(ctx, []string{"run", "hello"}, &stdout, &stderr)

	if exitCode != 130 {
		t.Fatalf("exit code = %d, want 130", exitCode)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q, want empty", stdout.String(), stderr.String())
	}
}

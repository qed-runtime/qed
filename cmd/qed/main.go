package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cli "github.com/mayahiro/nagicli-go"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/reload"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/internal/agentconfig"
	"github.com/qed-runtime/qed/internal/chatauth"
	"github.com/qed-runtime/qed/internal/cliapproval"
	"github.com/qed-runtime/qed/internal/extensionregistry"
	"github.com/qed-runtime/qed/internal/tuiapp"
	"github.com/qed-runtime/qed/provider/anthropic"
	"github.com/qed-runtime/qed/provider/echo"
	"github.com/qed-runtime/qed/provider/openai"
	"github.com/qed-runtime/qed/provider/openaicodex"
)

const (
	promptValueID            = "prompt"
	outputValueID            = "output"
	providerValueID          = "provider"
	modelValueID             = "model"
	baseURLValueID           = "base-url"
	instructionsValueID      = "system"
	maxOutputTokensValueID   = "max-output-tokens"
	authProfileValueID       = "auth-profile"
	deviceCodeValueID        = "device-code"
	noOpenValueID            = "no-open"
	configValueID            = "config"
	agentValueID             = "agent"
	workspaceValueID         = "workspace"
	approvalValueID          = "approval"
	sessionIDValueID         = "session-id"
	sessionIDArgumentID      = "session-id-argument"
	responseJSONValueID      = "response-json"
	evidenceStoreValueID     = "evidence-store"
	runIDArgumentID          = "run-id"
	evidenceDigestArgumentID = "evidence-digest"
	verboseValueID           = "verbose"
	extensionTargetID        = "extension-target"
	buildProgramValueID      = "build-program"
	buildArgumentValueID     = "build-arg"
	watchIntervalValueID     = "watch-interval"
	debounceValueID          = "debounce"
	controlDirectoryValueID  = "control-dir"
	extensionLockValueID     = "extension-lock"
	extensionOutputValueID   = "extension-output"
	extensionPackageValueID  = "extension-package"
	extensionVariableValueID = "extension-variable"
	checkGeneratedValueID    = "check-generated"

	providerEcho            = "echo"
	providerOpenAIResponses = "openai-responses"
	providerOpenAIChat      = "openai-chat"
	providerOpenAICodex     = "openai-codex"
	providerAnthropic       = "anthropic"
)

type chatAuthService interface {
	LoginBrowser(context.Context, string, chatauth.BrowserLoginOptions) (chatauth.ProfileInfo, error)
	LoginDevice(context.Context, string, chatauth.DeviceLoginOptions) (chatauth.ProfileInfo, error)
	Profiles(context.Context) ([]chatauth.ProfileInfo, error)
	Logout(context.Context, string) (chatauth.LogoutResult, error)
}

type commandDependencies struct {
	newRuntime         func(runtimeConfig) (*agent.Runtime, error)
	loadAgentConfig    func(string, agentconfig.LoadOptions) (*agentconfig.Configuration, error)
	runTUI             func(context.Context, tuiapp.StartFunc, agent.RunRequest, string) (tuiapp.Outcome, error)
	newChatAuthService func() (chatAuthService, error)
	openURL            func(context.Context, string) error
}

type runtimeConfig struct {
	provider        string
	model           string
	baseURL         string
	instructions    string
	maxOutputTokens int
	apiKey          string
	authProfile     string
	verbose         bool
	debugWriter     io.Writer
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) >= 2 && os.Args[1] == selfexec.ChildArgument {
		os.Exit(runExtension(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runExtension(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	handled, err := extensionregistry.Catalog.Dispatch(ctx, selfexec.DispatchOptions{
		Arguments:   arguments,
		Input:       stdin,
		Output:      stdout,
		DebugWriter: stderr,
	})
	if !handled || errors.Is(err, selfexec.ErrInvalidInvocation) {
		_, _ = fmt.Fprintln(stderr, "invalid internal Extension invocation")
		return int(cli.StatusUsage)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "serve Extension: %v\n", err)
		return int(cli.StatusFailure)
	}
	return int(cli.StatusSuccess)
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	return runWithInputAndDependencies(ctx, arguments, os.Stdin, stdout, stderr, defaultCommandDependencies())
}

func runWithDependencies(
	ctx context.Context,
	arguments []string,
	stdout, stderr io.Writer,
	dependencies commandDependencies,
) int {
	return runWithInputAndDependencies(ctx, arguments, os.Stdin, stdout, stderr, dependencies)
}

func runWithInputAndDependencies(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	dependencies commandDependencies,
) int {
	currentDirectory, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "determine current directory: %v\n", err)
		return int(cli.StatusFailure)
	}

	serializedStderr := &synchronizedWriter{writer: stderr}
	commandContext := cli.NewContextWithCancellation(
		stdin,
		stdout,
		serializedStderr,
		processEnvironment(os.Environ()),
		currentDirectory,
		ctx,
	)
	outcome, err := application(dependencies).Run(commandContext, arguments)
	if err != nil {
		_, _ = fmt.Fprintf(serializedStderr, "run command: %v\n", err)
		return int(cli.StatusFailure)
	}
	return int(outcome.Status())
}

func defaultCommandDependencies() commandDependencies {
	return commandDependencies{
		newRuntime: func(config runtimeConfig) (*agent.Runtime, error) {
			var modelProvider agent.Provider
			switch config.provider {
			case providerEcho:
				modelProvider = echo.New()
			case providerOpenAIResponses:
				configured, err := openai.New(openai.Config{
					API:             openai.APIResponses,
					APIKey:          config.apiKey,
					BaseURL:         config.baseURL,
					Model:           config.model,
					MaxOutputTokens: config.maxOutputTokens,
				})
				if err != nil {
					return nil, err
				}
				modelProvider = configured
			case providerOpenAIChat:
				configured, err := openai.New(openai.Config{
					API:             openai.APIChatCompletions,
					APIKey:          config.apiKey,
					BaseURL:         config.baseURL,
					Model:           config.model,
					MaxOutputTokens: config.maxOutputTokens,
				})
				if err != nil {
					return nil, err
				}
				modelProvider = configured
			case providerOpenAICodex:
				authService, err := chatauth.NewDefault()
				if err != nil {
					return nil, err
				}
				source, err := authService.CredentialSource(config.authProfile)
				if err != nil {
					return nil, err
				}
				configured, err := openaicodex.New(openaicodex.Config{
					AuthorizationSource: source,
					Model:               config.model,
				})
				if err != nil {
					return nil, err
				}
				modelProvider = configured
			case providerAnthropic:
				configured, err := anthropic.New(anthropic.Config{
					APIKey:          config.apiKey,
					BaseURL:         config.baseURL,
					Model:           config.model,
					MaxOutputTokens: config.maxOutputTokens,
				})
				if err != nil {
					return nil, err
				}
				modelProvider = configured
			default:
				return nil, fmt.Errorf("unsupported provider %q", config.provider)
			}
			return agent.NewRuntime(agent.Options{
				Provider: modelProvider,
				Logger:   newDebugLogger(config.verbose, config.debugWriter, "runtime"),
			})
		},
		loadAgentConfig: agentconfig.Load,
		runTUI:          tuiapp.RunWithStarterOutcome,
		newChatAuthService: func() (chatAuthService, error) {
			return chatauth.NewDefault()
		},
		openURL: openBrowserURL,
	}
}

func application(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("qed").
		About("Run embeddable QED agents").
		Option(
			cli.Flag(verboseValueID).
				Long("verbose").
				Help("Write structured debug diagnostics to stderr"),
		).
		RequireSubcommand().
		Subcommand(runAgentCommand(dependencies)).
		Subcommand(runTUICommand(dependencies)).
		Subcommand(authCommand(dependencies)).
		Subcommand(sessionCommand(dependencies)).
		Subcommand(evidenceCommand()).
		Subcommand(cacheCommand()).
		Subcommand(extensionCommand())
}

func authCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("auth").
		About("Manage named ChatGPT subscription credentials").
		RequireSubcommand().
		Subcommand(authLoginCommand(dependencies)).
		Subcommand(authStatusCommand(dependencies)).
		Subcommand(authLogoutCommand(dependencies))
}

func authLoginCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("login").
		About("Sign in to ChatGPT for the openai-codex Provider").
		Option(defaultAuthProfileOption()).
		Option(
			cli.Flag(deviceCodeValueID).
				Long("device-code").
				Help("Use the headless device-code flow"),
		).
		Option(
			cli.Flag(noOpenValueID).
				Long("no-open").
				Help("Print the browser login URL without opening it"),
		).
		Validator(func(invocation *cli.Invocation) *cli.Diagnostic {
			deviceCode, _ := invocation.Flag(deviceCodeValueID)
			noOpen, _ := invocation.Flag(noOpenValueID)
			if deviceCode && noOpen {
				return cli.NewDiagnostic(cli.CodeValidation, "--no-open cannot be used with --device-code").
					WithCategory(cli.CategoryUsage).
					WithTarget(cli.OptionTarget(noOpenValueID))
			}
			return nil
		}).
		Example("browser login", "qed auth login --auth-profile personal").
		Example("headless login", "qed auth login --auth-profile server --device-code").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			profileID, diagnostic := requiredString(invocation, authProfileValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			service, err := dependencies.newChatAuthService()
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("configure ChatGPT auth: %v", err))
			}
			deviceCode, _ := invocation.Flag(deviceCodeValueID)
			var profile chatauth.ProfileInfo
			if deviceCode {
				profile, err = service.LoginDevice(
					commandContext.Cancellation(),
					profileID,
					chatauth.DeviceLoginOptions{PresentCode: func(code chatauth.DeviceCode) {
						_, _ = fmt.Fprintf(
							commandContext.Stdout(),
							"Open %s and enter code %s\n",
							code.VerificationURL,
							code.UserCode,
						)
					}},
				)
			} else {
				noOpen, _ := invocation.Flag(noOpenValueID)
				profile, err = service.LoginBrowser(
					commandContext.Cancellation(),
					profileID,
					chatauth.BrowserLoginOptions{PresentURL: func(authorizationURL string) {
						_, _ = fmt.Fprintf(commandContext.Stdout(), "Open this URL to sign in:\n%s\n", authorizationURL)
						if noOpen || dependencies.openURL == nil {
							return
						}
						if openErr := dependencies.openURL(commandContext.Cancellation(), authorizationURL); openErr != nil {
							_, _ = fmt.Fprintf(commandContext.Stderr(), "warning: could not open browser: %v\n", openErr)
						}
					}},
				)
			}
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("sign in to ChatGPT: %v", err))
			}
			_, err = fmt.Fprintf(commandContext.Stdout(), "Logged in ChatGPT auth profile %q", profile.ID)
			if err == nil && profile.Email != "" {
				_, err = fmt.Fprintf(commandContext.Stdout(), " as %s", profile.Email)
			}
			if err == nil {
				_, err = fmt.Fprintln(commandContext.Stdout())
			}
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write login result: %v", err))
			}
			return cli.Success(), nil
		})
}

func authStatusCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("status").
		About("List ChatGPT auth profiles without exposing credentials").
		Option(authProfileOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			service, err := dependencies.newChatAuthService()
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("configure ChatGPT auth: %v", err))
			}
			profiles, err := service.Profiles(commandContext.Cancellation())
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("read ChatGPT auth profiles: %v", err))
			}
			selected := optionalString(invocation, authProfileValueID)
			found := false
			for _, profile := range profiles {
				if selected != "" && profile.ID != selected {
					continue
				}
				found = true
				status := "valid"
				if !profile.ExpiresAt.After(time.Now()) {
					status = "refresh-required"
				}
				email := profile.Email
				if email == "" {
					email = "-"
				}
				plan := profile.Plan
				if plan == "" {
					plan = "-"
				}
				if _, err := fmt.Fprintf(
					commandContext.Stdout(),
					"%s\t%s\texpires=%s\tplan=%s\temail=%s\n",
					profile.ID,
					status,
					profile.ExpiresAt.UTC().Format(time.RFC3339),
					plan,
					email,
				); err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write auth status: %v", err))
				}
			}
			if selected != "" && !found {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("ChatGPT auth profile %q is not logged in", selected))
			}
			if selected == "" && !found {
				if _, err := fmt.Fprintln(commandContext.Stdout(), "No ChatGPT auth profiles"); err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write auth status: %v", err))
				}
			}
			return cli.Success(), nil
		})
}

func authLogoutCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("logout").
		About("Remove one ChatGPT auth profile and revoke its token when possible").
		Option(defaultAuthProfileOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			profileID, diagnostic := requiredString(invocation, authProfileValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			service, err := dependencies.newChatAuthService()
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("configure ChatGPT auth: %v", err))
			}
			result, err := service.Logout(commandContext.Cancellation(), profileID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("log out ChatGPT auth profile: %v", err))
			}
			if !result.Removed {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("ChatGPT auth profile %q is not logged in", profileID))
			}
			if result.RevocationError != nil {
				_, _ = fmt.Fprintf(commandContext.Stderr(), "warning: local credentials were removed but token revocation failed: %v\n", result.RevocationError)
			}
			if _, err := fmt.Fprintf(commandContext.Stdout(), "Logged out ChatGPT auth profile %q\n", profileID); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write logout result: %v", err))
			}
			return cli.Success(), nil
		})
}

func authProfileOption() *cli.OptionSpec {
	return cli.ValueOption(authProfileValueID).
		Long("auth-profile").
		Parser(cli.StringParser()).
		Help("Named ChatGPT credential profile")
}

func defaultAuthProfileOption() *cli.OptionSpec {
	return authProfileOption().Default("default")
}

func openBrowserURL(ctx context.Context, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("browser URL must be an HTTPS URL without embedded credentials")
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", value)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", value)
	case "linux":
		command = exec.CommandContext(ctx, "xdg-open", value)
	default:
		return fmt.Errorf("opening a browser is not supported on %s", runtime.GOOS)
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}

func extensionCommand() *cli.Command {
	return cli.NewCommand("extension").
		About("Generate, develop, reload, and inspect Extensions").
		RequireSubcommand().
		Subcommand(generateExtensionCommand()).
		Subcommand(devExtensionCommand()).
		Subcommand(reloadExtensionCommand()).
		Subcommand(inspectExtensionCommand())
}

func generateExtensionCommand() *cli.Command {
	return cli.NewCommand("generate").
		About("Generate the self-exec Extension catalog from extensions.lock").
		Option(
			cli.ValueOption(extensionLockValueID).
				Long("lock").
				Parser(cli.StringParser()).
				Default(selfexec.LockFilename).
				Help("Extension lock file"),
		).
		Option(
			cli.ValueOption(extensionOutputValueID).
				Long("output").
				Parser(cli.StringParser()).
				Default("internal/extensionregistry/registry_gen.go").
				Help("Generated Go catalog file"),
		).
		Option(
			cli.ValueOption(extensionPackageValueID).
				Long("package").
				Parser(cli.StringParser()).
				Default(selfexec.DefaultGeneratedPackage).
				Help("Generated Go package name"),
		).
		Option(
			cli.ValueOption(extensionVariableValueID).
				Long("variable").
				Parser(cli.StringParser()).
				Default(selfexec.DefaultGeneratedVariable).
				Help("Exported generated Catalog variable name"),
		).
		Option(
			cli.Flag(checkGeneratedValueID).
				Long("check").
				Help("Verify that the generated catalog is current without writing"),
		).
		Example("Generate catalog", "qed extension generate").
		Example("Check catalog", "qed extension generate --check").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			lockValue, diagnostic := requiredString(invocation, extensionLockValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			outputValue, diagnostic := requiredString(invocation, extensionOutputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			packageValue, diagnostic := requiredString(invocation, extensionPackageValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			variableValue, diagnostic := requiredString(invocation, extensionVariableValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			lockPath := resolveCLIPath(commandContext, lockValue)
			outputPath := resolveCLIPath(commandContext, outputValue)
			source, err := selfexec.Generate(lockPath, selfexec.GenerateOptions{
				PackageName:  packageValue,
				VariableName: variableValue,
			})
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("generate Extension catalog: %v", err))
			}
			check, _ := invocation.Flag(checkGeneratedValueID)
			if check {
				current, err := selfexec.CheckGenerated(outputPath, source)
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("check Extension catalog: %v", err))
				}
				if !current {
					return cli.Outcome{}, cli.NewDiagnostic(
						cli.CodeHandlerError,
						fmt.Sprintf("generated Extension catalog %q is stale", outputValue),
					)
				}
				if _, err := fmt.Fprintf(commandContext.Stdout(), "Extension catalog %s is current\n", outputValue); err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Extension catalog status: %v", err))
				}
				return cli.Success(), nil
			}
			if err := selfexec.WriteGenerated(outputPath, source); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("write Extension catalog: %v", err))
			}
			if _, err := fmt.Fprintf(commandContext.Stdout(), "Generated Extension catalog %s from %s\n", outputValue, lockValue); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Extension catalog status: %v", err))
			}
			return cli.Success(), nil
		})
}

func devExtensionCommand() *cli.Command {
	return cli.NewCommand("dev").
		About("Build, host, watch, and atomically reload one Extension").
		Argument(
			cli.Positional(extensionTargetID).
				Parser(cli.StringParser()).
				Required().
				Help("Extension directory or manifest path"),
		).
		Option(
			cli.ValueOption(buildProgramValueID).
				Long("build-program").
				Parser(cli.StringParser()).
				Default("go").
				Help("Build executable invoked directly without a shell"),
		).
		Option(
			cli.ValueOption(buildArgumentValueID).
				Long("build-arg").
				Parser(cli.StringParser()).
				Repeated().
				Help("Repeated build argument; custom arguments must include {output}"),
		).
		Option(
			cli.ValueOption(watchIntervalValueID).
				Long("watch-interval").
				Parser(cli.StringParser()).
				Default("500ms").
				Help("Source polling interval"),
		).
		Option(
			cli.ValueOption(debounceValueID).
				Long("debounce").
				Parser(cli.StringParser()).
				Default("250ms").
				Help("Delay before an automatic rebuild"),
		).
		Option(extensionControlDirectoryOption()).
		Example("Go Extension", "qed extension dev ./extensions/my-extension").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			target, diagnostic := requiredString(invocation, extensionTargetID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			resolved, err := manifest.LoadForDevelopment(resolveCLIPath(commandContext, target))
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Extension manifest: %v", err))
			}
			allow := make([]capability.Name, len(resolved.Manifest.Capabilities))
			for index, name := range resolved.Manifest.Capabilities {
				allow[index] = capability.Name(name)
			}
			policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: allow})
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("configure Extension Policy: %v", err))
			}
			interval, err := parseDurationOption(invocation, watchIntervalValueID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, err.Error())
			}
			debounce, err := parseDurationOption(invocation, debounceValueID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, err.Error())
			}
			buildArguments := repeatedStrings(invocation, buildArgumentValueID)
			controlDirectory := extensionControlDirectory(commandContext, invocation)
			if err := reload.Dev(commandContext.Cancellation(), reload.DevOptions{
				ManifestPath:     resolved.Path,
				BuildProgram:     optionalString(invocation, buildProgramValueID),
				BuildArgs:        buildArguments,
				BuildEnvironment: environmentList(commandContext.EnvironmentValues()),
				Policy:           policy,
				StateStore:       extension.NewMemoryStateStore(),
				StateScope:       "development",
				ControlDirectory: controlDirectory,
				StatusWriter:     commandContext.Stdout(),
				WatchInterval:    interval,
				Debounce:         debounce,
				Verbose:          verboseEnabled(invocation),
				DebugWriter:      commandContext.Stderr(),
			}); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("develop Extension: %v", err))
			}
			return cli.Success(), nil
		})
}

func reloadExtensionCommand() *cli.Command {
	return cli.NewCommand("reload").
		About("Request an atomic rebuild and reload from a running development host").
		Argument(
			cli.Positional(extensionTargetID).
				Parser(cli.StringParser()).
				Required().
				Help("Extension ID"),
		).
		Option(extensionControlDirectoryOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			extensionID, diagnostic := requiredString(invocation, extensionTargetID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			status, err := reload.RequestControl(
				commandContext.Cancellation(),
				extensionControlDirectory(commandContext, invocation),
				extensionID,
				"reload",
			)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("reload Extension: %v", err))
			}
			_, err = fmt.Fprintf(commandContext.Stdout(), "Extension %s generation %d version %s\n", status.ExtensionID, status.Generation, status.Version)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Extension status: %v", err))
			}
			return cli.Success(), nil
		})
}

func inspectExtensionCommand() *cli.Command {
	return cli.NewCommand("inspect").
		About("Inspect a manifest or a running Extension development host").
		Argument(
			cli.Positional(extensionTargetID).
				Parser(cli.StringParser()).
				Required().
				Help("Extension ID, directory, or manifest path"),
		).
		Option(extensionControlDirectoryOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			target, diagnostic := requiredString(invocation, extensionTargetID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			path := resolveCLIPath(commandContext, target)
			if _, err := os.Stat(path); err == nil {
				resolved, err := manifest.Load(path)
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Extension manifest: %v", err))
				}
				encoder := json.NewEncoder(commandContext.Stdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(struct {
					Manifest   manifest.Manifest `json:"manifest"`
					Path       string            `json:"path"`
					Directory  string            `json:"directory"`
					Entrypoint string            `json:"entrypoint"`
				}{resolved.Manifest, resolved.Path, resolved.Directory, resolved.Entrypoint}); err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Extension manifest: %v", err))
				}
				return cli.Success(), nil
			}
			status, err := reload.RequestControl(
				commandContext.Cancellation(),
				extensionControlDirectory(commandContext, invocation),
				target,
				"status",
			)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("inspect Extension: %v", err))
			}
			encoder := json.NewEncoder(commandContext.Stdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(status); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Extension status: %v", err))
			}
			return cli.Success(), nil
		})
}

func extensionControlDirectoryOption() *cli.OptionSpec {
	return cli.ValueOption(controlDirectoryValueID).
		Long("control-dir").
		Parser(cli.StringParser()).
		Help("Private development-host control directory")
}

func extensionControlDirectory(commandContext *cli.Context, invocation *cli.Invocation) string {
	value := optionalString(invocation, controlDirectoryValueID)
	if value == "" {
		return filepath.Join(commandContext.CurrentDirectory(), ".qed", "extension-dev")
	}
	return resolveCLIPath(commandContext, value)
}

func resolveCLIPath(commandContext *cli.Context, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(commandContext.CurrentDirectory(), value)
}

func parseDurationOption(invocation *cli.Invocation, id string) (time.Duration, error) {
	value, diagnostic := requiredString(invocation, id)
	if diagnostic != nil {
		return 0, errors.New(diagnostic.Message())
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("--%s must be a non-negative duration", id)
	}
	return duration, nil
}

func repeatedStrings(invocation *cli.Invocation, id string) []string {
	values := invocation.ParsedValues(id)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if typed, ok := value.Typed().(string); ok {
			result = append(result, typed)
		}
	}
	return result
}

func environmentList(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = name + "=" + environment[name]
	}
	return result
}

func verboseEnabled(invocation *cli.Invocation) bool {
	value, _ := invocation.Flag(verboseValueID)
	return value
}

func newDebugLogger(enabled bool, writer io.Writer, component string) *slog.Logger {
	if !enabled || writer == nil {
		return nil
	}
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelDebug})).With(
		"component", component,
	)
}

func runAgentCommand(dependencies commandDependencies) *cli.Command {
	command := cli.NewCommand("run").
		About("Run one non-interactive Agent turn").
		Option(
			cli.ValueOption(promptValueID).
				Long("prompt").
				Parser(cli.StringParser()).
				Help("User prompt"),
		).
		Option(
			cli.ValueOption(outputValueID).
				Long("output").
				Parser(cli.PossibleValuesParser("text", "jsonl")).
				Default("text").
				Help("Output format"),
		).
		Option(
			cli.ValueOption(configValueID).
				Long("config").
				Parser(cli.StringParser()).
				ConflictsSupplied(providerValueID).
				ConflictsSupplied(modelValueID).
				ConflictsSupplied(baseURLValueID).
				ConflictsSupplied(instructionsValueID).
				ConflictsSupplied(authProfileValueID).
				ConflictsSupplied(maxOutputTokensValueID).
				Help("JSON file containing Providers, execution Profiles, and Agents"),
		).
		Option(
			cli.ValueOption(agentValueID).
				Long("agent").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Configured Agent ID, overriding default_agent"),
		).
		Option(
			cli.ValueOption(workspaceValueID).
				Long("workspace").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Workspace root for configured Profiles, defaulting to the current directory"),
		).
		Option(
			cli.ValueOption(approvalValueID).
				Long("approval").
				Parser(cli.PossibleValuesParser("deny", "prompt")).
				Default("deny").
				RequiresSupplied(configValueID).
				Help("Approval handling for ask capabilities"),
		).
		Option(
			cli.ValueOption(sessionIDValueID).
				Long("session-id").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Persist this turn in the configured Session Store"),
		)
	command = withProviderOptions(command).
		Validator(validatePrompt).
		Validator(validateAgentConfigOptions).
		Validator(validateProviderOptions).
		Example("text output", "qed run --prompt hello").
		Example("configured Agent", "qed run --config qed.json --agent coordinator --workspace . --prompt hello").
		Example("interactive approval", "qed run --config qed.json --approval prompt --prompt hello").
		Example("OpenAI response", "qed run --provider openai-responses --model MODEL --prompt hello").
		Example("ChatGPT Codex response", "qed run --provider openai-codex --auth-profile personal --model MODEL --prompt hello").
		Example("event output", "qed run --prompt hello --output jsonl").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			prompt, diagnostic := requiredString(invocation, promptValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			output, diagnostic := requiredString(invocation, outputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}

			configPath := optionalString(invocation, configValueID)
			if configPath != "" {
				var approver capability.Approver
				var terminalApprover *cliapproval.Approver
				if optionalString(invocation, approvalValueID) == "prompt" {
					configuredApprover, err := cliapproval.New(commandContext.Stdin(), commandContext.Stderr())
					if err != nil {
						return cli.Outcome{}, cli.NewDiagnostic(
							cli.CodeHandlerError,
							fmt.Sprintf("configure approval prompt: %v", err),
						)
					}
					terminalApprover = configuredApprover
					approver = capability.WaitApprover{}
				}
				workspaceRoot := optionalString(invocation, workspaceValueID)
				if workspaceRoot == "" {
					workspaceRoot = commandContext.CurrentDirectory()
				}
				selfExecutable, err := os.Executable()
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(
						cli.CodeHandlerError,
						fmt.Sprintf("resolve QED executable: %v", err),
					)
				}
				selfExecutable, err = filepath.Abs(selfExecutable)
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(
						cli.CodeHandlerError,
						fmt.Sprintf("resolve absolute QED executable: %v", err),
					)
				}
				configured, err := dependencies.loadAgentConfig(configPath, agentconfig.LoadOptions{
					LookupEnv:       commandContext.Environment,
					WorkspaceRoot:   workspaceRoot,
					SelfExecutable:  selfExecutable,
					SelfExecCatalog: extensionregistry.Catalog,
					Context:         commandContext.Cancellation(),
					Approver:        approver,
					Verbose:         verboseEnabled(invocation),
					DebugWriter:     commandContext.Stderr(),
				})
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(
						cli.CodeHandlerError,
						fmt.Sprintf("load Agent configuration: %v", err),
					)
				}
				agentID, err := configured.ResolveAgent(optionalString(invocation, agentValueID))
				if err != nil {
					_ = configured.Close()
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, err.Error())
				}
				sessionID := optionalString(invocation, sessionIDValueID)
				if sessionID != "" && configured.SessionStore == nil {
					_ = configured.Close()
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, "configuration has no Session Store")
				}
				runErr := executeConfiguredAgentRun(
					commandContext.Cancellation(),
					configured,
					agentID,
					sessionID,
					prompt,
					output,
					commandContext.Stdout(),
					terminalApprover,
				)
				closeErr := configured.Close()
				if runErr != nil {
					return cli.Outcome{}, runErr
				}
				if closeErr != nil {
					return cli.Outcome{}, cli.NewDiagnostic(
						cli.CodeHandlerError,
						fmt.Sprintf("close Agent configuration: %v", closeErr),
					)
				}
				return cli.Success(), nil
			}

			config, diagnostic := runtimeConfiguration(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			runtime, err := dependencies.newRuntime(config)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(
					cli.CodeHandlerError,
					fmt.Sprintf("configure runtime: %v", err),
				)
			}
			if err := executeAgentRun(
				commandContext.Cancellation(),
				runtime,
				config.provider,
				config.instructions,
				prompt,
				output,
				commandContext.Stdout(),
			); err != nil {
				return cli.Outcome{}, err
			}
			return cli.Success(), nil
		}).
		Subcommand(inspectEvidenceCommand()).
		Subcommand(exportEvidenceCommand())
	return command
}

func runTUICommand(dependencies commandDependencies) *cli.Command {
	command := cli.NewCommand("tui").
		About("Run one Agent turn in an interactive terminal").
		Option(
			cli.ValueOption(promptValueID).
				Long("prompt").
				Parser(cli.StringParser()).
				Required().
				Help("User prompt"),
		).
		Option(
			cli.ValueOption(configValueID).
				Long("config").
				Parser(cli.StringParser()).
				ConflictsSupplied(providerValueID).
				ConflictsSupplied(modelValueID).
				ConflictsSupplied(baseURLValueID).
				ConflictsSupplied(instructionsValueID).
				ConflictsSupplied(authProfileValueID).
				ConflictsSupplied(maxOutputTokensValueID).
				Help("JSON file containing Providers, execution Profiles, and Agents"),
		).
		Option(
			cli.ValueOption(agentValueID).
				Long("agent").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Configured Agent ID, overriding default_agent"),
		).
		Option(
			cli.ValueOption(workspaceValueID).
				Long("workspace").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Workspace root for configured Profiles, defaulting to the current directory"),
		).
		Option(
			cli.ValueOption(sessionIDValueID).
				Long("session-id").
				Parser(cli.StringParser()).
				RequiresSupplied(configValueID).
				Help("Persist this turn in the configured Session Store"),
		)
	command = withProviderOptions(command).
		Validator(validatePrompt).
		Validator(validateAgentConfigOptions).
		Validator(validateProviderOptions).
		Example("interactive event view", "qed tui --prompt hello").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			prompt, diagnostic := requiredString(invocation, promptValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			configPath := optionalString(invocation, configValueID)
			if configPath != "" {
				workspaceRoot := optionalString(invocation, workspaceValueID)
				if workspaceRoot == "" {
					workspaceRoot = commandContext.CurrentDirectory()
				}
				selfExecutable, err := os.Executable()
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("resolve QED executable: %v", err))
				}
				selfExecutable, err = filepath.Abs(selfExecutable)
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("resolve absolute QED executable: %v", err))
				}
				configured, err := dependencies.loadAgentConfig(configPath, agentconfig.LoadOptions{
					LookupEnv:       commandContext.Environment,
					WorkspaceRoot:   workspaceRoot,
					SelfExecutable:  selfExecutable,
					SelfExecCatalog: extensionregistry.Catalog,
					Context:         commandContext.Cancellation(),
					Approver:        capability.WaitApprover{},
					Verbose:         verboseEnabled(invocation),
					DebugWriter:     commandContext.Stderr(),
				})
				if err != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Agent configuration: %v", err))
				}
				agentID, err := configured.ResolveAgent(optionalString(invocation, agentValueID))
				if err != nil {
					_ = configured.Close()
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, err.Error())
				}
				sessionID := optionalString(invocation, sessionIDValueID)
				if sessionID != "" && configured.SessionStore == nil {
					_ = configured.Close()
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, "configuration has no Session Store")
				}
				outcome, runErr := dependencies.runTUI(
					commandContext.Cancellation(),
					configured.Registry.Start,
					agent.RunRequest{
						AgentID:   agentID,
						SessionID: sessionID,
						Input:     []agent.Message{{Role: agent.RoleUser, Text: prompt}},
					},
					prompt,
				)
				if configured.EvidenceStore != nil && outcome.Result.RunID != "" {
					evidenceContext, cancel := context.WithTimeout(context.WithoutCancel(commandContext.Cancellation()), 5*time.Second)
					_, evidenceErr := configured.SaveRunEvidence(evidenceContext, outcome.Result, outcome.Events)
					cancel()
					if evidenceErr != nil {
						runErr = errors.Join(runErr, fmt.Errorf("save Run Evidence: %w", evidenceErr))
					}
				}
				closeErr := configured.Close()
				if runErr != nil {
					if errors.Is(runErr, context.Canceled) {
						return cli.Outcome{}, cli.NewDiagnostic(cli.CodeCancelled, "TUI canceled")
					}
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("run TUI: %v", runErr))
				}
				if closeErr != nil {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("close Agent configuration: %v", closeErr))
				}
				return cli.Success(), nil
			}

			config, diagnostic := runtimeConfiguration(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			runtime, err := dependencies.newRuntime(config)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(
					cli.CodeHandlerError,
					fmt.Sprintf("configure runtime: %v", err),
				)
			}
			if _, err := dependencies.runTUI(
				commandContext.Cancellation(),
				runtime.Run,
				agent.RunRequest{
					AgentID:      config.provider,
					Instructions: config.instructions,
					Input:        []agent.Message{{Role: agent.RoleUser, Text: prompt}},
				},
				prompt,
			); err != nil {
				if errors.Is(err, context.Canceled) {
					return cli.Outcome{}, cli.NewDiagnostic(cli.CodeCancelled, "TUI canceled")
				}
				return cli.Outcome{}, cli.NewDiagnostic(
					cli.CodeHandlerError,
					fmt.Sprintf("run TUI: %v", err),
				)
			}
			return cli.Success(), nil
		})
	return command
}

func sessionCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("session").
		About("Inspect or resume persisted Agent Sessions").
		RequireSubcommand().
		Subcommand(resumeSessionCommand(dependencies))
}

func evidenceCommand() *cli.Command {
	return cli.NewCommand("evidence").
		About("Inspect persisted Run Evidence and content-addressed objects").
		RequireSubcommand().
		Subcommand(inspectEvidenceCommand()).
		Subcommand(exportEvidenceCommand()).
		Subcommand(fetchEvidenceCommand())
}

func fetchEvidenceCommand() *cli.Command {
	return cli.NewCommand("fetch").
		About("Fetch one content-addressed Evidence Object").
		Argument(
			cli.Positional(evidenceDigestArgumentID).
				Parser(cli.StringParser()).
				Required().
				Help("Evidence Object sha256 digest"),
		).
		Option(evidenceStoreOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			digest, diagnostic := requiredString(invocation, evidenceDigestArgumentID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			content, err := store.GetObject(commandContext.Cancellation(), agent.EvidenceObjectRef{Digest: digest})
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("fetch Evidence Object: %v", err))
			}
			if _, err := commandContext.Stdout().Write(content); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Evidence Object: %v", err))
			}
			return cli.Success(), nil
		})
}

func inspectEvidenceCommand() *cli.Command {
	return evidenceReadCommand("inspect", "Inspect one Run Evidence summary", func(
		commandContext *cli.Context,
		bundle evidence.Bundle,
	) error {
		_, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Run: %s\nStatus: %s\nAgent: %s\nModel: %s\nEvents: %d\nTools: %d\nUsage: input=%d output=%d total=%d cost_micros=%d\n",
			bundle.Run.ID,
			bundle.Run.Status,
			bundle.Agent.ID,
			bundle.Model.Name,
			len(bundle.Events),
			len(bundle.ToolTrace),
			bundle.Usage.InputTokens,
			bundle.Usage.OutputTokens,
			bundle.Usage.TotalTokens,
			bundle.Usage.CostMicros,
		)
		return err
	})
}

func exportEvidenceCommand() *cli.Command {
	return evidenceReadCommand("export", "Export one complete Run Evidence Bundle as JSON", func(
		commandContext *cli.Context,
		bundle evidence.Bundle,
	) error {
		encoder := json.NewEncoder(commandContext.Stdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(bundle)
	})
}

func evidenceReadCommand(
	name string,
	about string,
	write func(*cli.Context, evidence.Bundle) error,
) *cli.Command {
	return cli.NewCommand(name).
		About(about).
		Argument(
			cli.Positional(runIDArgumentID).
				Parser(cli.StringParser()).
				Required().
				Help("Run ID"),
		).
		Option(evidenceStoreOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			runID, diagnostic := requiredString(invocation, runIDArgumentID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			bundle, err := store.Load(commandContext.Cancellation(), runID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Run Evidence: %v", err))
			}
			if err := write(commandContext, bundle); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Run Evidence: %v", err))
			}
			return cli.Success(), nil
		})
}

func cacheCommand() *cli.Command {
	return cli.NewCommand("cache").
		About("Inspect Provider prompt cache plans and usage").
		RequireSubcommand().
		Subcommand(cacheStatusCommand())
}

func cacheStatusCommand() *cli.Command {
	return cli.NewCommand("status").
		About("Show cache planning and usage for one stored Run").
		Argument(
			cli.Positional(runIDArgumentID).
				Parser(cli.StringParser()).
				Help("Run ID, defaulting to the newest stored Bundle"),
		).
		Option(evidenceStoreOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			bundle, err := loadCacheStatusBundle(
				commandContext.Cancellation(),
				store,
				optionalString(invocation, runIDArgumentID),
			)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load cache status: %v", err))
			}
			if err := writeCacheStatus(commandContext, store, bundle); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write cache status: %v", err))
			}
			return cli.Success(), nil
		})
}

func evidenceStoreOption() *cli.OptionSpec {
	return cli.ValueOption(evidenceStoreValueID).
		Long("store").
		Parser(cli.StringParser()).
		Default(".qed/evidence").
		Help("Evidence Store directory")
}

func openEvidenceStore(
	commandContext *cli.Context,
	invocation *cli.Invocation,
) (*evidence.JSONStore, *cli.Diagnostic) {
	storePath, diagnostic := requiredString(invocation, evidenceStoreValueID)
	if diagnostic != nil {
		return nil, diagnostic
	}
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(commandContext.CurrentDirectory(), storePath)
	}
	store, err := evidence.NewJSONStore(storePath)
	if err != nil {
		return nil, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("open Evidence Store: %v", err))
	}
	return store, nil
}

func loadCacheStatusBundle(ctx context.Context, store *evidence.JSONStore, runID string) (evidence.Bundle, error) {
	if runID != "" {
		return store.Load(ctx, runID)
	}
	descriptors, err := store.List(ctx)
	if err != nil {
		return evidence.Bundle{}, err
	}
	if len(descriptors) == 0 {
		return evidence.Bundle{}, errors.New("Evidence Store has no Run Bundles")
	}
	var newest evidence.Bundle
	for _, descriptor := range descriptors {
		bundle, err := store.Load(ctx, descriptor.ID)
		if err != nil {
			return evidence.Bundle{}, err
		}
		if newest.Run.ID == "" || bundle.CreatedAt.After(newest.CreatedAt) ||
			(bundle.CreatedAt.Equal(newest.CreatedAt) && bundle.Run.ID > newest.Run.ID) {
			newest = bundle
		}
	}
	return newest, nil
}

func writeCacheStatus(commandContext *cli.Context, store *evidence.JSONStore, bundle evidence.Bundle) error {
	currentEvent, currentIndex := latestCacheEvent(bundle.Events)
	if currentEvent == nil || currentEvent.CachePlan == nil {
		_, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Run: %s\nCache plan: unavailable\nUsage: input=%d output=%d total=%d\n",
			bundle.Run.ID,
			bundle.Usage.InputTokens,
			bundle.Usage.OutputTokens,
			bundle.Usage.TotalTokens,
		)
		return err
	}
	plan := currentEvent.CachePlan
	manifest := currentEvent.PrefixManifest
	providerName := ""
	model := bundle.Model.Name
	if manifest != nil {
		providerName = manifest.Provider
		if manifest.Model != "" {
			model = manifest.Model
		}
	}
	if _, err := fmt.Fprintf(
		commandContext.Stdout(),
		"Run: %s\nProvider: %s\nModel: %s\nMode: %s\nCache family: %s\nTTL: %s\nBreakpoints: %d\nInput estimate: %d tokens (%s)\n",
		bundle.Run.ID,
		providerName,
		model,
		plan.Mode,
		plan.FamilyID,
		plan.TTL,
		len(plan.Breakpoints),
		plan.InputTokenEstimate,
		plan.TokenEstimateKind,
	); err != nil {
		return err
	}
	if plan.FallbackReason != "" {
		if _, err := fmt.Fprintf(commandContext.Stdout(), "Fallback: %s\n", plan.FallbackReason); err != nil {
			return err
		}
	}
	if bundle.Usage.InputTokenDetailsReported {
		ratio := float64(0)
		if bundle.Usage.InputTokens > 0 {
			ratio = 100 * float64(bundle.Usage.CacheReadInputTokens) / float64(bundle.Usage.InputTokens)
		}
		if _, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Usage: uncached=%d cache_read=%d cache_write=%d output=%d cache_read_ratio=%.2f%%\n",
			bundle.Usage.UncachedInputTokens,
			bundle.Usage.CacheReadInputTokens,
			bundle.Usage.CacheWriteInputTokens,
			bundle.Usage.OutputTokens,
			ratio,
		); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(
		commandContext.Stdout(),
		"Usage: input=%d output=%d total=%d cache_details=unreported\n",
		bundle.Usage.InputTokens,
		bundle.Usage.OutputTokens,
		bundle.Usage.TotalTokens,
	); err != nil {
		return err
	}
	if plan.Forecast != nil {
		if _, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Forecast: currency=%s without_cache_micros=%d with_cache_micros=%d savings_micros=%d expected_uses=%d\n",
			plan.Forecast.Currency,
			plan.Forecast.WithoutCacheMicros,
			plan.Forecast.WithCacheMicros,
			plan.Forecast.SavingsMicros,
			plan.Forecast.ExpectedUses,
		); err != nil {
			return err
		}
	}
	if plan.Pricing != nil {
		cost, err := agent.EstimateUsageCost(*plan.Pricing, bundle.Usage)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Estimated actual cost: currency=%s input_micros=%d output_micros=%d total_micros=%d\n",
			cost.Currency,
			cost.InputMicros,
			cost.OutputMicros,
			cost.TotalMicros,
		); err != nil {
			return err
		}
	}
	previous := previousManifestInEvents(bundle.Events, currentIndex)
	if previous == nil && manifest != nil {
		var err error
		previous, err = previousStoredManifest(commandContext.Cancellation(), store, bundle, *manifest)
		if err != nil {
			return err
		}
	}
	if manifest != nil {
		divergence := firstManifestDivergence(previous, manifest)
		if _, err := fmt.Fprintf(commandContext.Stdout(), "First divergence: %s\n", divergence); err != nil {
			return err
		}
	}
	for index := len(bundle.Events) - 1; index >= 0; index-- {
		report := bundle.Events[index].ContextCompaction
		if report == nil {
			continue
		}
		_, err := fmt.Fprintf(
			commandContext.Stdout(),
			"Compaction: reason=%s original_bytes=%d compiled_bytes=%d source_messages=%d recent_messages=%d\n",
			report.Reason,
			report.OriginalBytes,
			report.CompiledBytes,
			report.SourceMessageCount,
			report.RecentMessageCount,
		)
		return err
	}
	return nil
}

func latestCacheEvent(events []agent.Event) (*agent.Event, int) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == agent.EventModelRequest && events[index].CachePlan != nil {
			event := events[index]
			return &event, index
		}
	}
	return nil, -1
}

func previousManifestInEvents(events []agent.Event, before int) *agent.PrefixManifest {
	for index := before - 1; index >= 0; index-- {
		if events[index].PrefixManifest != nil {
			return events[index].PrefixManifest
		}
	}
	return nil
}

func previousStoredManifest(
	ctx context.Context,
	store *evidence.JSONStore,
	current evidence.Bundle,
	manifest agent.PrefixManifest,
) (*agent.PrefixManifest, error) {
	descriptors, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	var selected evidence.Bundle
	var selectedManifest *agent.PrefixManifest
	for _, descriptor := range descriptors {
		if descriptor.ID == current.Run.ID {
			continue
		}
		bundle, err := store.Load(ctx, descriptor.ID)
		if err != nil {
			return nil, err
		}
		earlier := bundle.CreatedAt.Before(current.CreatedAt) ||
			(bundle.CreatedAt.Equal(current.CreatedAt) && bundle.Run.ID < current.Run.ID)
		if !earlier {
			continue
		}
		event, _ := latestCacheEvent(bundle.Events)
		if event == nil || event.PrefixManifest == nil || event.PrefixManifest.Provider != manifest.Provider ||
			event.PrefixManifest.Model != manifest.Model || event.PrefixManifest.CacheFamily != manifest.CacheFamily {
			continue
		}
		if selected.Run.ID == "" || bundle.CreatedAt.After(selected.CreatedAt) ||
			(bundle.CreatedAt.Equal(selected.CreatedAt) && bundle.Run.ID > selected.Run.ID) {
			selected = bundle
			selectedManifest = event.PrefixManifest
		}
	}
	return selectedManifest, nil
}

func firstManifestDivergence(previous, current *agent.PrefixManifest) string {
	if previous == nil {
		return "none (no earlier manifest in this cache family)"
	}
	maximum := min(len(previous.Segments), len(current.Segments))
	for index := 0; index < maximum; index++ {
		if previous.Segments[index] != current.Segments[index] {
			return current.Segments[index].ID
		}
	}
	if len(previous.Segments) == len(current.Segments) {
		return "none"
	}
	if len(current.Segments) > maximum {
		return current.Segments[maximum].ID + " (append-only)"
	}
	return previous.Segments[maximum].ID + " (removed)"
}

func resumeSessionCommand(dependencies commandDependencies) *cli.Command {
	return cli.NewCommand("resume").
		About("Resume one persisted pending Run").
		Argument(
			cli.Positional(sessionIDArgumentID).
				Parser(cli.StringParser()).
				Required().
				Help("Persisted Session ID"),
		).
		Option(
			cli.ValueOption(configValueID).
				Long("config").
				Parser(cli.StringParser()).
				Required().
				Help("JSON file containing the Session Store and Agent"),
		).
		Option(
			cli.ValueOption(agentValueID).
				Long("agent").
				Parser(cli.StringParser()).
				Help("Configured Agent ID, overriding default_agent"),
		).
		Option(
			cli.ValueOption(workspaceValueID).
				Long("workspace").
				Parser(cli.StringParser()).
				Help("Workspace root for configured Profiles, defaulting to the current directory"),
		).
		Option(
			cli.ValueOption(outputValueID).
				Long("output").
				Parser(cli.PossibleValuesParser("text", "jsonl")).
				Default("text").
				Help("Output format"),
		).
		Option(
			cli.ValueOption(approvalValueID).
				Long("approval").
				Parser(cli.PossibleValuesParser("prompt", "approve", "deny")).
				Default("prompt").
				Help("Response for a persisted approval request"),
		).
		Option(
			cli.ValueOption(responseJSONValueID).
				Long("response-json").
				Parser(cli.StringParser()).
				Help("Exact JSON payload for a non-approval wait request"),
		).
		Example("interactive approval", "qed session resume SESSION --config qed.json").
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			sessionID, diagnostic := requiredString(invocation, sessionIDArgumentID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			if strings.TrimSpace(sessionID) != sessionID || sessionID == "" {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, "Session ID is required and must not have surrounding whitespace")
			}
			configPath, diagnostic := requiredString(invocation, configValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			output, diagnostic := requiredString(invocation, outputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}

			workspaceRoot := optionalString(invocation, workspaceValueID)
			if workspaceRoot == "" {
				workspaceRoot = commandContext.CurrentDirectory()
			}
			selfExecutable, err := os.Executable()
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("resolve QED executable: %v", err))
			}
			selfExecutable, err = filepath.Abs(selfExecutable)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("resolve absolute QED executable: %v", err))
			}
			configured, err := dependencies.loadAgentConfig(configPath, agentconfig.LoadOptions{
				LookupEnv:       commandContext.Environment,
				WorkspaceRoot:   workspaceRoot,
				SelfExecutable:  selfExecutable,
				SelfExecCatalog: extensionregistry.Catalog,
				Context:         commandContext.Cancellation(),
				Approver:        capability.WaitApprover{},
				Verbose:         verboseEnabled(invocation),
				DebugWriter:     commandContext.Stderr(),
			})
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Agent configuration: %v", err))
			}
			defer configured.Close()
			if configured.SessionStore == nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, "configuration has no Session Store")
			}
			snapshot, err := configured.SessionStore.Load(commandContext.Cancellation(), sessionID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Session: %v", err))
			}
			if snapshot.PendingWait == nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, "Session has no pending wait request")
			}
			responsePayload, err := sessionResumePayload(commandContext, invocation, *snapshot.PendingWait)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, err.Error())
			}
			agentID, err := configured.ResolveAgent(optionalString(invocation, agentValueID))
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, err.Error())
			}
			handle, err := configured.Registry.Start(commandContext.Cancellation(), agent.RunRequest{
				AgentID:   agentID,
				SessionID: sessionID,
				Resume: &agent.WaitResponse{
					RequestID: snapshot.PendingWait.ID,
					Payload:   responsePayload,
				},
			})
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("resume Session: %v", err))
			}
			if err := executeConfiguredRunHandle(commandContext.Cancellation(), configured, handle, output, commandContext.Stdout(), nil); err != nil {
				return cli.Outcome{}, err
			}
			return cli.Success(), nil
		})
}

func sessionResumePayload(
	commandContext *cli.Context,
	invocation *cli.Invocation,
	wait agent.WaitRequest,
) (json.RawMessage, error) {
	if invocation.Supplied(responseJSONValueID) {
		value := optionalString(invocation, responseJSONValueID)
		if !json.Valid([]byte(value)) {
			return nil, errors.New("--response-json must contain one valid JSON value")
		}
		return json.RawMessage(value), nil
	}
	if wait.Kind != agent.WaitKindApproval {
		return nil, fmt.Errorf("wait kind %q requires --response-json", wait.Kind)
	}
	mode := optionalString(invocation, approvalValueID)
	approved := mode == "approve"
	if mode == "prompt" {
		terminalApprover, err := cliapproval.New(commandContext.Stdin(), commandContext.Stderr())
		if err != nil {
			return nil, fmt.Errorf("configure approval prompt: %w", err)
		}
		request, err := approvalRequestFromWait(wait)
		if err != nil {
			return nil, err
		}
		approved, err = terminalApprover.Approve(commandContext.Cancellation(), request)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		Approved bool `json:"approved"`
	}{Approved: approved})
}

func withProviderOptions(command *cli.Command) *cli.Command {
	return command.
		Option(
			cli.ValueOption(providerValueID).
				Long("provider").
				Parser(cli.PossibleValuesParser(
					providerEcho,
					providerOpenAIResponses,
					providerOpenAIChat,
					providerOpenAICodex,
					providerAnthropic,
				)).
				Default(providerEcho).
				Help("Model Provider and API dialect"),
		).
		Option(
			cli.ValueOption(modelValueID).
				Long("model").
				Parser(cli.StringParser()).
				Help("Exact model identifier for a model-backed Provider"),
		).
		Option(
			cli.ValueOption(baseURLValueID).
				Long("base-url").
				Parser(cli.StringParser()).
				Help("Trusted custom API base URL"),
		).
		Option(
			cli.ValueOption(authProfileValueID).
				Long("auth-profile").
				Parser(cli.StringParser()).
				Help("Named ChatGPT credential profile for openai-codex"),
		).
		Option(
			cli.ValueOption(instructionsValueID).
				Long("system").
				Parser(cli.StringParser()).
				Help("System-level instruction"),
		).
		Option(
			cli.ValueOption(maxOutputTokensValueID).
				Long("max-output-tokens").
				Parser(cli.IntegerParser()).
				Default("0").
				Help("Output token limit, or 0 for the Provider default"),
		)
}

func validatePrompt(invocation *cli.Invocation) *cli.Diagnostic {
	path := invocation.CommandPath()
	if len(path) > 0 && path[len(path)-1] != "run" {
		return nil
	}
	prompt, ok := cli.ValueAs[string](invocation, promptValueID)
	if !ok {
		return cli.NewDiagnostic(cli.CodeMissingRequired, `required option "--prompt" is missing`).
			WithCategory(cli.CategoryUsage).
			WithTarget(cli.OptionTarget(promptValueID))
	}
	if strings.TrimSpace(prompt) != "" {
		return nil
	}
	return cli.NewDiagnostic(cli.CodeValidation, "--prompt must not be empty").
		WithCategory(cli.CategoryUsage).
		WithTarget(cli.OptionTarget(promptValueID))
}

func validateProviderOptions(invocation *cli.Invocation) *cli.Diagnostic {
	if invocation.Supplied(configValueID) {
		return nil
	}
	providerName, ok := cli.ValueAs[string](invocation, providerValueID)
	if !ok {
		return nil
	}
	model, _ := cli.ValueAs[string](invocation, modelValueID)
	if providerName != providerEcho && strings.TrimSpace(model) == "" {
		return cli.NewDiagnostic(cli.CodeValidation, "--model is required for the selected provider").
			WithCategory(cli.CategoryUsage).
			WithTarget(cli.OptionTarget(modelValueID))
	}
	maxOutputTokens, ok := cli.ValueAs[int64](invocation, maxOutputTokensValueID)
	if ok && maxOutputTokens < 0 {
		return cli.NewDiagnostic(cli.CodeValidation, "--max-output-tokens must not be negative").
			WithCategory(cli.CategoryUsage).
			WithTarget(cli.OptionTarget(maxOutputTokensValueID))
	}
	authProfile, _ := cli.ValueAs[string](invocation, authProfileValueID)
	if providerName == providerOpenAICodex {
		if strings.TrimSpace(authProfile) == "" {
			return cli.NewDiagnostic(cli.CodeValidation, "--auth-profile is required for openai-codex").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(authProfileValueID))
		}
		if strings.TrimSpace(authProfile) != authProfile {
			return cli.NewDiagnostic(cli.CodeValidation, "--auth-profile must not have surrounding whitespace").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(authProfileValueID))
		}
		if invocation.Supplied(baseURLValueID) {
			return cli.NewDiagnostic(cli.CodeValidation, "--base-url is not supported by openai-codex").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(baseURLValueID))
		}
		if maxOutputTokens > 0 {
			return cli.NewDiagnostic(cli.CodeValidation, "--max-output-tokens is not supported by openai-codex").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(maxOutputTokensValueID))
		}
	} else if invocation.Supplied(authProfileValueID) {
		return cli.NewDiagnostic(cli.CodeValidation, "--auth-profile is only supported by openai-codex").
			WithCategory(cli.CategoryUsage).
			WithTarget(cli.OptionTarget(authProfileValueID))
	}
	return nil
}

func validateAgentConfigOptions(invocation *cli.Invocation) *cli.Diagnostic {
	if invocation.Supplied(configValueID) {
		configPath, _ := cli.ValueAs[string](invocation, configValueID)
		if strings.TrimSpace(configPath) == "" {
			return cli.NewDiagnostic(cli.CodeValidation, "--config must not be empty").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(configValueID))
		}
	}
	if invocation.Supplied(agentValueID) {
		agentID, _ := cli.ValueAs[string](invocation, agentValueID)
		if strings.TrimSpace(agentID) == "" {
			return cli.NewDiagnostic(cli.CodeValidation, "--agent must not be empty").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(agentValueID))
		}
	}
	if invocation.Supplied(workspaceValueID) {
		workspaceRoot, _ := cli.ValueAs[string](invocation, workspaceValueID)
		if strings.TrimSpace(workspaceRoot) == "" {
			return cli.NewDiagnostic(cli.CodeValidation, "--workspace must not be empty").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(workspaceValueID))
		}
	}
	if invocation.Supplied(sessionIDValueID) {
		sessionID, _ := cli.ValueAs[string](invocation, sessionIDValueID)
		if strings.TrimSpace(sessionID) != sessionID || sessionID == "" {
			return cli.NewDiagnostic(cli.CodeValidation, "--session-id is required and must not have surrounding whitespace").
				WithCategory(cli.CategoryUsage).
				WithTarget(cli.OptionTarget(sessionIDValueID))
		}
	}
	return nil
}

func runtimeConfiguration(
	commandContext *cli.Context,
	invocation *cli.Invocation,
) (runtimeConfig, *cli.Diagnostic) {
	providerName, diagnostic := requiredString(invocation, providerValueID)
	if diagnostic != nil {
		return runtimeConfig{}, diagnostic
	}
	maxOutputTokens, accessErr := cli.RequireValueAs[int64](invocation, maxOutputTokensValueID)
	if accessErr != nil {
		return runtimeConfig{}, cli.NewDiagnostic(cli.CodeHandlerError, accessErr.Error()).
			WithTarget(cli.OptionTarget(maxOutputTokensValueID))
	}
	if strconv.IntSize == 32 && maxOutputTokens > int64(^uint(0)>>1) {
		return runtimeConfig{}, cli.NewDiagnostic(cli.CodeValidation, "--max-output-tokens is too large").
			WithCategory(cli.CategoryUsage).
			WithTarget(cli.OptionTarget(maxOutputTokensValueID))
	}

	config := runtimeConfig{
		provider:        providerName,
		model:           optionalString(invocation, modelValueID),
		baseURL:         optionalString(invocation, baseURLValueID),
		instructions:    optionalString(invocation, instructionsValueID),
		maxOutputTokens: int(maxOutputTokens),
		authProfile:     optionalString(invocation, authProfileValueID),
		verbose:         verboseEnabled(invocation),
		debugWriter:     commandContext.Stderr(),
	}

	var keyEnvironment string
	switch providerName {
	case providerOpenAIResponses, providerOpenAIChat:
		keyEnvironment = "OPENAI_API_KEY"
	case providerAnthropic:
		keyEnvironment = "ANTHROPIC_API_KEY"
	}
	if keyEnvironment != "" {
		if strings.TrimSpace(config.baseURL) == "" {
			config.apiKey, _ = commandContext.Environment(keyEnvironment)
		} else {
			config.apiKey, _ = commandContext.Environment("QED_API_KEY")
		}
		if strings.TrimSpace(config.apiKey) == "" && strings.TrimSpace(config.baseURL) == "" {
			return runtimeConfig{}, cli.NewDiagnostic(
				cli.CodeHandlerError,
				keyEnvironment+" is required for the default API endpoint",
			)
		}
	}
	return config, nil
}

func optionalString(invocation *cli.Invocation, valueID string) string {
	value, _ := cli.ValueAs[string](invocation, valueID)
	return value
}

func requiredString(invocation *cli.Invocation, valueID string) (string, *cli.Diagnostic) {
	value, accessErr := cli.RequireValueAs[string](invocation, valueID)
	if accessErr == nil {
		return value, nil
	}
	return "", cli.NewDiagnostic(cli.CodeHandlerError, accessErr.Error()).
		WithTarget(cli.OptionTarget(valueID))
}

func executeAgentRun(
	ctx context.Context,
	runtime *agent.Runtime,
	agentID, instructions, prompt, output string,
	stdout io.Writer,
) error {
	handle, err := runtime.Run(ctx, agent.RunRequest{
		AgentID:      agentID,
		Instructions: instructions,
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: prompt},
		},
	})
	if err != nil {
		return cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("start run: %v", err))
	}
	_, runErr := executeRunHandle(ctx, handle, output, stdout, nil)
	return runErr
}

func executeConfiguredAgentRun(
	ctx context.Context,
	configuration *agentconfig.Configuration,
	agentID, sessionID, prompt, output string,
	stdout io.Writer,
	approver *cliapproval.Approver,
) error {
	handle, err := configuration.Registry.Start(ctx, agent.RunRequest{
		AgentID:   agentID,
		SessionID: sessionID,
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: prompt},
		},
	})
	if err != nil {
		return cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("start configured run: %v", err))
	}
	return executeConfiguredRunHandle(ctx, configuration, handle, output, stdout, approver)
}

type runExecution struct {
	result agent.RunResult
	events []agent.Event
}

func executeRunHandle(
	ctx context.Context,
	handle *agent.RunHandle,
	output string,
	stdout io.Writer,
	approver *cliapproval.Approver,
) (runExecution, error) {

	var outputErr error
	var interactionErr error
	var events []agent.Event
	if output == "jsonl" {
		encoder := json.NewEncoder(stdout)
		for event := range handle.Events() {
			events = append(events, event)
			if outputErr == nil {
				if err := encoder.Encode(event); err != nil {
					outputErr = fmt.Errorf("write event: %w", err)
					handle.Cancel()
				}
			}
			if interactionErr == nil {
				interactionErr = resolveRunWait(ctx, handle, event, approver)
				if interactionErr != nil {
					handle.Cancel()
				}
			}
		}
	} else {
		for event := range handle.Events() {
			events = append(events, event)
			if interactionErr == nil {
				interactionErr = resolveRunWait(ctx, handle, event, approver)
			}
			if interactionErr != nil {
				handle.Cancel()
			}
		}
	}

	result, runErr := handle.Wait()
	execution := runExecution{result: result, events: events}
	if outputErr != nil {
		return execution, cli.NewDiagnostic(cli.CodeIOError, outputErr.Error())
	}
	if interactionErr != nil {
		return execution, cli.NewDiagnostic(cli.CodeHandlerError, interactionErr.Error())
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return execution, cli.NewDiagnostic(cli.CodeCancelled, "run canceled")
		}
		return execution, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("run failed: %v", runErr))
	}

	if output == "text" {
		message, ok := lastAssistantMessage(result.Messages)
		if !ok {
			return execution, cli.NewDiagnostic(
				cli.CodeHandlerError,
				"run completed without an assistant message",
			)
		}
		if _, err := fmt.Fprintln(stdout, message.Text); err != nil {
			return execution, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write output: %v", err))
		}
	}

	return execution, nil
}

func executeConfiguredRunHandle(
	ctx context.Context,
	configuration *agentconfig.Configuration,
	handle *agent.RunHandle,
	output string,
	stdout io.Writer,
	approver *cliapproval.Approver,
) error {
	execution, runErr := executeRunHandle(ctx, handle, output, stdout, approver)
	if configuration.EvidenceStore != nil && execution.result.RunID != "" {
		evidenceContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, evidenceErr := configuration.SaveRunEvidence(evidenceContext, execution.result, execution.events)
		cancel()
		if evidenceErr != nil {
			if runErr != nil {
				return cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("%v; save Run Evidence: %v", runErr, evidenceErr))
			}
			return cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("save Run Evidence: %v", evidenceErr))
		}
	}
	return runErr
}

func resolveRunWait(
	ctx context.Context,
	handle *agent.RunHandle,
	event agent.Event,
	approver *cliapproval.Approver,
) error {
	if event.Type != agent.EventRunWaiting || event.WaitRequest == nil {
		return nil
	}
	if event.WaitRequest.Kind != agent.WaitKindApproval {
		return fmt.Errorf("Run is waiting for unsupported input kind %q", event.WaitRequest.Kind)
	}
	if approver == nil {
		return errors.New("Run requested approval but interactive approval is disabled")
	}
	request, err := approvalRequestFromWait(*event.WaitRequest)
	if err != nil {
		return err
	}
	approved, err := approver.Approve(ctx, request)
	if err != nil {
		return err
	}
	response, err := json.Marshal(struct {
		Approved bool `json:"approved"`
	}{Approved: approved})
	if err != nil {
		return fmt.Errorf("encode approval response: %w", err)
	}
	if err := handle.Resume(agent.WaitResponse{
		RequestID: event.WaitRequest.ID,
		Payload:   response,
	}); err != nil {
		return fmt.Errorf("resume Run approval: %w", err)
	}
	return nil
}

func approvalRequestFromWait(wait agent.WaitRequest) (capability.Request, error) {
	var payload struct {
		Tool         string            `json:"tool"`
		Capabilities []capability.Name `json:"capabilities"`
	}
	if err := json.Unmarshal(wait.Payload, &payload); err != nil {
		return capability.Request{}, fmt.Errorf("decode approval request: %w", err)
	}
	return capability.Request{
		Tool:         payload.Tool,
		Capabilities: payload.Capabilities,
	}, nil
}

func lastAssistantMessage(messages []agent.Message) (agent.Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == agent.RoleAssistant {
			return messages[index], true
		}
	}
	return agent.Message{}, false
}

func processEnvironment(entries []string) map[string]string {
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[name] = value
		}
	}
	return environment
}

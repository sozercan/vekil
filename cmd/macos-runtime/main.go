package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/internal/macosruntime"
	"github.com/sozercan/vekil/logger"
)

// Set by the bundle build with:
//
//	-X main.buildVersion=<marketing-version>
//	-X main.bundleBuildID=<manifest-bundle_build_id>
var (
	buildVersion  = "dev"
	bundleBuildID = "dev"
)

type options struct {
	host      string
	port      string
	tokenDir  string
	stateDir  string
	parentPID int
	logLevel  string
}

func main() {
	configureProcessGroup()
	if code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	log := logger.NewWithWriter(logger.ParseLevel(opts.logLevel), stderr)
	paths := macosruntime.Paths{}
	if opts.stateDir != "" {
		paths = macosruntime.PathsInDirectory(opts.stateDir)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stateLock, err := macosruntime.AcquireHelperStateLock(ctx, paths)
	if err != nil {
		log.Error("helper initialization failed", logger.F("code", "state_lock_failed"))
		return 1
	}
	defer func() {
		if err := stateLock.Close(); err != nil {
			log.Error("helper state lock release failed", logger.F("code", "state_lock_release_failed"))
		}
	}()

	authenticator, err := auth.NewAuthenticator(opts.tokenDir)
	if err != nil {
		log.Error("helper initialization failed", logger.F("code", "auth_init_failed"))
		return 1
	}
	authenticator.DisableAutoDeviceFlow = true

	configuration, err := macosruntime.NewConfigManager(macosruntime.ConfigManagerOptions{Paths: paths})
	if err != nil {
		log.Error("helper initialization failed", logger.F("code", "config_state_failed"))
		return 1
	}
	secrets := macosruntime.NewSecretProjectionStore()
	factory, err := macosruntime.NewRuntimeFactory(macosruntime.RuntimeFactoryOptions{
		Authenticator: authenticator,
		Logger:        log,
		Host:          opts.host,
		Port:          opts.port,
		Secrets:       secrets,
	})
	if err != nil {
		log.Error("helper initialization failed", logger.F("code", "runtime_factory_failed"))
		return 1
	}
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource:   configuration,
		ConfigurationObserver: configuration,
		RuntimeFactory:        factory,
		Authenticator:         authenticator,
		ReadinessChecker:      appcontrol.HTTPReadinessChecker{},
	})
	if err != nil {
		log.Error("helper initialization failed", logger.F("code", "controller_init_failed"))
		return 1
	}

	if err := macosruntime.RunHelper(ctx, macosruntime.HelperOptions{
		Stdin:              stdin,
		Stdout:             stdout,
		Stderr:             stderr,
		Controller:         controller,
		Configuration:      configuration,
		Secrets:            secrets,
		Authenticator:      authenticator,
		CandidateValidator: factory,
		ProtocolMin:        macosruntime.ProtocolMin,
		ProtocolMax:        macosruntime.ProtocolMax,
		HelperBuild:        buildVersion,
		BundleBuildID:      bundleBuildID,
		ParentPID:          opts.parentPID,
	}); err != nil {
		log.Error("runtime helper exited", logger.F("code", "helper_failed"))
		return 1
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("vekil-runtime", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.host, "host", macosruntime.DefaultHost, "proxy listen host")
	fs.StringVar(&opts.port, "port", macosruntime.DefaultPort, "proxy listen port (0 selects an ephemeral port)")
	fs.StringVar(&opts.tokenDir, "token-dir", envOrDefault("TOKEN_DIR", ""), "token storage directory")
	fs.StringVar(&opts.stateDir, "state-dir", "", "helper state directory override")
	fs.IntVar(&opts.parentPID, "parent-pid", envInt("VEKIL_PARENT_PID", 0), "native shell parent process id")
	fs.StringVar(&opts.logLevel, "log-level", envOrDefault("LOG_LEVEL", "info"), "debug, info, or error")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return opts, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

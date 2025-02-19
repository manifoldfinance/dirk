// Copyright © 2020, 2021 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	// #nosec G108
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"github.com/attestantio/dirk/cmd"
	"github.com/attestantio/dirk/core"
	"github.com/attestantio/dirk/rules"
	standardrules "github.com/attestantio/dirk/rules/standard"
	standardaccountmanager "github.com/attestantio/dirk/services/accountmanager/standard"
	grpcapi "github.com/attestantio/dirk/services/api/grpc"
	"github.com/attestantio/dirk/services/checker"
	staticchecker "github.com/attestantio/dirk/services/checker/static"
	"github.com/attestantio/dirk/services/fetcher"
	memfetcher "github.com/attestantio/dirk/services/fetcher/mem"
	"github.com/attestantio/dirk/services/lister"
	standardlister "github.com/attestantio/dirk/services/lister/standard"
	"github.com/attestantio/dirk/services/locker"
	syncmaplocker "github.com/attestantio/dirk/services/locker/syncmap"
	"github.com/attestantio/dirk/services/metrics"
	prometheusmetrics "github.com/attestantio/dirk/services/metrics/prometheus"
	"github.com/attestantio/dirk/services/peers"
	staticpeers "github.com/attestantio/dirk/services/peers/static"
	standardprocess "github.com/attestantio/dirk/services/process/standard"
	"github.com/attestantio/dirk/services/ruler"
	goruler "github.com/attestantio/dirk/services/ruler/golang"
	sendergrpc "github.com/attestantio/dirk/services/sender/grpc"
	standardsigner "github.com/attestantio/dirk/services/signer/standard"
	"github.com/attestantio/dirk/services/unlocker"
	localunlocker "github.com/attestantio/dirk/services/unlocker/local"
	standardwalletmanager "github.com/attestantio/dirk/services/walletmanager/standard"
	"github.com/attestantio/dirk/util"
	"github.com/attestantio/dirk/util/loggers"
	"github.com/mitchellh/go-homedir"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	zerologger "github.com/rs/zerolog/log"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	jaegerconfig "github.com/uber/jaeger-client-go/config"
	e2types "github.com/wealdtech/go-eth2-types/v2"
	e2wtypes "github.com/wealdtech/go-eth2-wallet-types/v2"
	majordomo "github.com/wealdtech/go-majordomo"
	directconfidant "github.com/wealdtech/go-majordomo/confidants/direct"
	fileconfidant "github.com/wealdtech/go-majordomo/confidants/file"
	gsmconfidant "github.com/wealdtech/go-majordomo/confidants/gsm"
	standardmajordomo "github.com/wealdtech/go-majordomo/standard"
)

// ReleaseVersion is the release version for the code.
var ReleaseVersion = "1.1.0-pre-3"

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	if err := fetchConfig(); err != nil {
		zerologger.Fatal().Err(err).Msg("Failed to fetch configuration")
	}

	majordomo, err := initMajordomo(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialise majordomo")
	}

	// runCommands will not return if a command is run.
	runCommands(ctx, majordomo)

	if err := initLogging(); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialise logging")
	}

	if viper.GetString("server.name") == "" {
		log.Fatal().Err(err).Msg("No server name set; cannot start")
	}

	logModules()
	log.Info().Str("version", ReleaseVersion).Msg("Starting dirk")

	if err := initProfiling(); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialise profiling")
	}

	closer, err := initTracing()
	if err != nil {
		log.Error().Err(err).Msg("Failed to initialise tracing")
		return
	}
	if closer != nil {
		defer closer.Close()
	}

	runtime.GOMAXPROCS(runtime.NumCPU() * 8)

	if err := e2types.InitBLS(); err != nil {
		log.Error().Err(err).Msg("Failed to initialise BLS library")
		return
	}

	monitor, err := startMonitor(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start metrics service")
		return
	}
	if err := registerMetrics(ctx, monitor); err != nil {
		log.Error().Err(err).Msg("Failed to register metrics")
		return
	}
	setRelease(ctx, ReleaseVersion)
	setReady(ctx, false)

	err = startServices(ctx, majordomo, monitor)
	if err != nil {
		log.Error().Err(err).Msg("Failed to initialise services")
		return
	}
	setReady(ctx, true)

	log.Info().Msg("All services operational")

	// Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	for {
		sig := <-sigCh
		if sig == syscall.SIGINT || sig == syscall.SIGTERM || sig == os.Interrupt || sig == os.Kill {
			cancel()
			break
		}
	}

	log.Info().Msg("Stopping dirk")
	setReady(ctx, false)

	// Give services a chance to stop cleanly before we exit.
	time.Sleep(2 * time.Second)
}

// fetchConfig fetches configuration from various sources.
func fetchConfig() error {
	pflag.String("base-dir", "", "base directory for configuration files")
	pflag.String("log-level", "info", "minimum level of messsages to log")
	pflag.String("log-file", "", "redirect log output to a file")
	pflag.String("profile-address", "", "Address on which to run Go profile server")
	pflag.String("tracing-address", "", "Address to which to send tracing data")
	pflag.Bool("show-certificates", false, "show server certificates and exit")
	pflag.Bool("show-permissions", false, "show client permissions and exit")
	pflag.Bool("version", false, "show Dirk version exit")
	pflag.Bool("export-slashing-protection", false, "export slashing protection data and exit")
	pflag.Bool("import-slashing-protection", false, "import slashing protection data and exit")
	pflag.String("genesis-validators-root", "", "genesis validators root required for slashing protection import or export")
	pflag.String("slashing-protection-file", "", "location of slashing protection file for import or export")
	pflag.Parse()
	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		return errors.Wrap(err, "failed to bind pflags to viper")
	}

	if viper.GetString("base-dir") != "" {
		// User-defined base directory.
		viper.AddConfigPath(viper.GetString("base-dir"))
		viper.SetConfigName("dirk")
	} else {
		// Home directory.
		home, err := homedir.Dir()
		if err != nil {
			return errors.Wrap(err, "failed to obtain home directory")
		}
		viper.AddConfigPath(home)
		viper.SetConfigName(".dirk")
	}

	// Environment settings.
	viper.SetEnvPrefix("DIRK")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	viper.AutomaticEnv()

	// Defaults.
	viper.SetDefault("storage-path", "storage")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return errors.Wrap(err, "failed to read configuration file")
		}
	}

	return nil
}

// initProfiling initialises the profiling server.
func initProfiling() error {
	profileAddress := viper.GetString("profile-address")
	if profileAddress != "" {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		log.Info().Str("profileAddress", profileAddress).Msg("Starting profile server")
		go func() {
			if err := http.ListenAndServe(profileAddress, nil); err != nil {
				log.Error().Str("profileAddress", profileAddress).Err(err).Msg("Failed to start profile server")
			}
		}()
	}
	return nil
}

// initTracing initialises the tracing.
func initTracing() (io.Closer, error) {
	tracingAddress := viper.GetString("tracing-address")
	if tracingAddress == "" {
		return nil, nil
	}
	cfg := &jaegerconfig.Configuration{
		ServiceName: "dirk",
		Sampler: &jaegerconfig.SamplerConfig{
			Type:  "const",
			Param: 1,
		},
		Reporter: &jaegerconfig.ReporterConfig{
			LogSpans:           true,
			LocalAgentHostPort: tracingAddress,
		},
	}
	tracer, closer, err := cfg.NewTracer(jaegerconfig.Logger(loggers.NewJaegerLogger(log)))
	if err != nil {
		return nil, err
	}
	if tracer != nil {
		opentracing.SetGlobalTracer(tracer)
	}

	return closer, nil
}

func runCommands(ctx context.Context, majordomo majordomo.Service) {
	if viper.GetBool("version") {
		fmt.Printf("%s\n", ReleaseVersion)
		os.Exit(0)
	}

	if viper.GetBool("show-certificates") {
		err := cmd.ShowCertificates(ctx, majordomo)
		if err != nil {
			log.Fatal().Err(err).Msg("show-certificates failed")
		}
		os.Exit(0)
	}

	if viper.GetBool("show-permissions") {
		permissionsCfg := viper.GetStringMap("permissions")
		permissions := make(map[string][]*checker.Permissions)
		for client := range permissionsCfg {
			perms := viper.GetStringMapStringSlice(fmt.Sprintf("permissions.%s", client))
			permissions[client] = make([]*checker.Permissions, 0, len(perms))
			for path, operations := range perms {
				permissions[client] = append(permissions[client], &checker.Permissions{
					Path:       path,
					Operations: operations,
				})
			}
		}
		checker.DumpPermissions(permissions)
		os.Exit(0)
	}

	if viper.GetBool("export-slashing-protection") {
		exportSlashingProtection(ctx)
	}

	if viper.GetBool("import-slashing-protection") {
		importSlashingProtection(ctx)
	}
}

func startServices(ctx context.Context, majordomo majordomo.Service, monitor metrics.Service) error {
	var err error

	stores, err := initStores(ctx)
	if err != nil {
		return err
	}

	unlocker, err := startUnlocker(ctx, majordomo, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to initialise local unlocker")
	}

	checker, err := startChecker(ctx, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to start permissions checker")
	}

	// Set up the fetcher.
	fetcher, err := startFetcher(ctx, stores, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to initialise account fetcher")
	}

	// Set up the locker.
	locker, err := startLocker(ctx, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to set up locker service")
	}

	// Set up the ruler.
	ruler, err := startRuler(ctx, locker, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to set up ruler service")
	}

	// Set up the lister.
	lister, err := startLister(ctx, monitor, fetcher, checker, ruler)
	if err != nil {
		return errors.Wrap(err, "failed to initialise lister")
	}

	// Set up the signer.
	var signerMonitor metrics.SignerMonitor
	if monitor, isMonitor := monitor.(metrics.SignerMonitor); isMonitor {
		signerMonitor = monitor
	}
	signer, err := standardsigner.New(ctx,
		standardsigner.WithLogLevel(util.LogLevel("signer")),
		standardsigner.WithMonitor(signerMonitor),
		standardsigner.WithUnlocker(unlocker),
		standardsigner.WithChecker(checker),
		standardsigner.WithFetcher(fetcher),
		standardsigner.WithRuler(ruler),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create signer service")
	}

	peers, err := startPeers(ctx, monitor)
	if err != nil {
		return errors.Wrap(err, "failed to start peers service")
	}

	var senderMonitor metrics.SenderMonitor
	if monitor, isMonitor := monitor.(metrics.SenderMonitor); isMonitor {
		senderMonitor = monitor
	}
	certPEMBlock, err := majordomo.Fetch(ctx, viper.GetString("certificates.server-cert"))
	if err != nil {
		return errors.Wrap(err, "failed to obtain server certificate")
	}
	keyPEMBlock, err := majordomo.Fetch(ctx, viper.GetString("certificates.server-key"))
	if err != nil {
		return errors.Wrap(err, "failed to obtain server key")
	}
	var caPEMBlock []byte
	if viper.GetString("certificates.ca-cert") != "" {
		caPEMBlock, err = majordomo.Fetch(ctx, viper.GetString("certificates.ca-cert"))
		if err != nil {
			return errors.Wrap(err, "failed to obtain client CA certificate")
		}
	}
	sender, err := sendergrpc.New(ctx,
		sendergrpc.WithLogLevel(util.LogLevel("sender")),
		sendergrpc.WithMonitor(senderMonitor),
		sendergrpc.WithName(viper.GetString("server.name")),
		sendergrpc.WithServerCert(certPEMBlock),
		sendergrpc.WithServerKey(keyPEMBlock),
		sendergrpc.WithCACert(caPEMBlock),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create sender service")
	}

	serverID, err := strconv.ParseUint(viper.GetString("server.id"), 10, 64)
	if err != nil {
		return errors.Wrap(err, "failed to obtain server ID")
	}

	endpoints := make(map[uint64]string)
	for k, v := range viper.GetStringMapString("peers") {
		peerID, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			log.Error().Err(err).Str("peer_id", k).Msg("Invalid peer ID")
			continue
		}
		endpoints[peerID] = v
	}
	var processMonitor metrics.ProcessMonitor
	if monitor, isMonitor := monitor.(metrics.ProcessMonitor); isMonitor {
		processMonitor = monitor
	}

	var generationPassphrase []byte
	if viper.GetString("process.generation-passphrase") != "" {
		generationPassphrase, err = majordomo.Fetch(ctx, viper.GetString("process.generation-passphrase"))
		if err != nil {
			return errors.Wrap(err, "failed to obtain account generation passphrase for process")
		}
	}
	process, err := standardprocess.New(ctx,
		standardprocess.WithLogLevel(util.LogLevel("process")),
		standardprocess.WithMonitor(processMonitor),
		standardprocess.WithChecker(checker),
		standardprocess.WithFetcher(fetcher),
		standardprocess.WithUnlocker(unlocker),
		standardprocess.WithSender(sender),
		standardprocess.WithPeers(peers),
		standardprocess.WithID(serverID),
		standardprocess.WithStores(stores),
		standardprocess.WithGenerationPassphrase(generationPassphrase),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create process service")
	}

	var accountManagerMonitor metrics.AccountManagerMonitor
	if monitor, isMonitor := monitor.(metrics.AccountManagerMonitor); isMonitor {
		accountManagerMonitor = monitor
	}
	accountManager, err := standardaccountmanager.New(ctx,
		standardaccountmanager.WithLogLevel(util.LogLevel("accountmanager")),
		standardaccountmanager.WithMonitor(accountManagerMonitor),
		standardaccountmanager.WithUnlocker(unlocker),
		standardaccountmanager.WithChecker(checker),
		standardaccountmanager.WithFetcher(fetcher),
		standardaccountmanager.WithRuler(ruler),
		standardaccountmanager.WithProcess(process),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create account manager service")
	}

	var walletManagerMonitor metrics.WalletManagerMonitor
	if monitor, isMonitor := monitor.(metrics.WalletManagerMonitor); isMonitor {
		walletManagerMonitor = monitor
	}
	walletManager, err := standardwalletmanager.New(ctx,
		standardwalletmanager.WithLogLevel(util.LogLevel("walletmanager")),
		standardwalletmanager.WithMonitor(walletManagerMonitor),
		standardwalletmanager.WithUnlocker(unlocker),
		standardwalletmanager.WithChecker(checker),
		standardwalletmanager.WithFetcher(fetcher),
		standardwalletmanager.WithRuler(ruler),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create wallet manager service")
	}

	// Initialise the API service.
	var apiMonitor metrics.APIMonitor
	if monitor, isMonitor := monitor.(metrics.APIMonitor); isMonitor {
		apiMonitor = monitor
	}
	_, err = grpcapi.New(ctx,
		grpcapi.WithLogLevel(util.LogLevel("api")),
		grpcapi.WithMonitor(apiMonitor),
		grpcapi.WithSigner(signer),
		grpcapi.WithLister(lister),
		grpcapi.WithProcess(process),
		grpcapi.WithAccountManager(accountManager),
		grpcapi.WithWalletManager(walletManager),
		grpcapi.WithPeers(peers),
		grpcapi.WithName(viper.GetString("server.name")),
		grpcapi.WithID(serverID),
		grpcapi.WithServerCert(certPEMBlock),
		grpcapi.WithServerKey(keyPEMBlock),
		grpcapi.WithCACert(caPEMBlock),
		grpcapi.WithListenAddress(viper.GetString("server.listen-address")),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create API service")
	}

	return nil
}

func initMajordomo(ctx context.Context) (majordomo.Service, error) {
	majordomo, err := standardmajordomo.New(ctx,
		standardmajordomo.WithLogLevel(util.LogLevel("majordomo")),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create majordomo service")
	}

	directConfidant, err := directconfidant.New(ctx,
		directconfidant.WithLogLevel(util.LogLevel("majordomo.confidants.direct")),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create direct confidant")
	}
	if err := majordomo.RegisterConfidant(ctx, directConfidant); err != nil {
		return nil, errors.Wrap(err, "failed to register direct confidant")
	}

	fileConfidant, err := fileconfidant.New(ctx,
		fileconfidant.WithLogLevel(util.LogLevel("majordomo.confidants.file")),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create file confidant")
	}
	if err := majordomo.RegisterConfidant(ctx, fileConfidant); err != nil {
		return nil, errors.Wrap(err, "failed to register file confidant")
	}

	if viper.GetString("majordomo.gsm.credentials") != "" {
		gsmConfidant, err := gsmconfidant.New(ctx,
			gsmconfidant.WithLogLevel(util.LogLevel("majordomo.confidants.gsm")),
			gsmconfidant.WithCredentialsPath(resolvePath(viper.GetString("majordomo.gsm.credentials"))),
			gsmconfidant.WithProject(viper.GetString("majordomo.gsm.project")),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create Google secrets manager confidant")
		}
		if err := majordomo.RegisterConfidant(ctx, gsmConfidant); err != nil {
			return nil, errors.Wrap(err, "failed to register Google secrets manager confidant")
		}
	}

	return majordomo, nil
}

func startMonitor(ctx context.Context) (metrics.Service, error) {
	log.Trace().Msg("Starting metrics service")
	var monitor metrics.Service
	var err error
	if viper.GetString("metrics.listen-address") == "" {
		log.Debug().Msg("No metrics listen address supplied; monitor not starting")
		return nil, nil
	}
	monitor, err = prometheusmetrics.New(ctx,
		prometheusmetrics.WithLogLevel(util.LogLevel("metrics")),
		prometheusmetrics.WithAddress(viper.GetString("metrics.listen-address")),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start metrics service")
	}
	return monitor, nil
}

func logModules() {
	buildInfo, ok := debug.ReadBuildInfo()
	if ok {
		log.Trace().Str("path", buildInfo.Path).Msg("Main package")
		for _, dep := range buildInfo.Deps {
			log := log.Trace()
			if dep.Replace == nil {
				log = log.Str("path", dep.Path).Str("version", dep.Version)
			} else {
				log = log.Str("path", dep.Replace.Path).Str("version", dep.Replace.Version)
			}
			log.Msg("Dependency")
		}
	}
}

// initRules initialises a rules service.
func initRules(ctx context.Context) (rules.Service, error) {
	return standardrules.New(ctx,
		standardrules.WithLogLevel(util.LogLevel("rules")),
		standardrules.WithStoragePath(resolvePath(viper.GetString("storage-path"))),
		standardrules.WithAdminIPs(viper.GetStringSlice("server.rules.admin-ips")),
	)
}

func initStores(ctx context.Context) ([]e2wtypes.Store, error) {
	storesCfg := &core.Stores{}
	if err := viper.Unmarshal(&storesCfg); err != nil {
		return nil, errors.Wrap(err, "failed to obtain stores configuration")
	}
	stores, err := core.InitStores(ctx, storesCfg.Stores)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialise stores")
	}
	if len(stores) == 0 {
		return nil, errors.New("no stores")
	}
	return stores, nil
}

func startUnlocker(ctx context.Context, majordomo majordomo.Service, monitor metrics.Service) (unlocker.Service, error) {
	// Set up the unlocker.
	walletPassphrases := make([]string, 0)
	for _, key := range viper.GetStringSlice("unlocker.wallet-passphrases") {
		value, err := majordomo.Fetch(ctx, key)
		if err != nil {
			return nil, errors.Wrap(err, "failed to obtain wallet passphrase for unlocker")
		}
		walletPassphrases = append(walletPassphrases, string(value))
	}
	accountPassphrases := make([]string, 0)
	for _, key := range viper.GetStringSlice("unlocker.account-passphrases") {
		value, err := majordomo.Fetch(ctx, key)
		if err != nil {
			return nil, errors.Wrap(err, "failed to obtain account passphrase for unlocker")
		}
		accountPassphrases = append(accountPassphrases, string(value))
	}
	var unlockerMonitor metrics.UnlockerMonitor
	if monitor, isMonitor := monitor.(metrics.UnlockerMonitor); isMonitor {
		unlockerMonitor = monitor
	}
	return localunlocker.New(ctx,
		localunlocker.WithLogLevel(util.LogLevel("unlocker")),
		localunlocker.WithMonitor(unlockerMonitor),
		localunlocker.WithWalletPassphrases(walletPassphrases),
		localunlocker.WithAccountPassphrases(accountPassphrases),
	)
}

func startChecker(ctx context.Context, monitor metrics.Service) (checker.Service, error) {
	// Set up the checker.
	permissionsCfg := viper.GetStringMap("permissions")
	permissions := make(map[string][]*checker.Permissions)
	for client := range permissionsCfg {
		perms := viper.GetStringMapStringSlice(fmt.Sprintf("permissions.%s", client))
		permissions[client] = make([]*checker.Permissions, 0, len(perms))
		for path, operations := range perms {
			permissions[client] = append(permissions[client], &checker.Permissions{
				Path:       path,
				Operations: operations,
			})
		}
	}
	var checkerMonitor metrics.CheckerMonitor
	if monitor, isMonitor := monitor.(metrics.CheckerMonitor); isMonitor {
		checkerMonitor = monitor
	}
	return staticchecker.New(ctx,
		staticchecker.WithLogLevel(util.LogLevel("checker")),
		staticchecker.WithMonitor(checkerMonitor),
		staticchecker.WithPermissions(permissions),
	)
}

func startFetcher(ctx context.Context, stores []e2wtypes.Store, monitor metrics.Service) (fetcher.Service, error) {
	var fetcherMonitor metrics.FetcherMonitor
	if monitor, isMonitor := monitor.(metrics.FetcherMonitor); isMonitor {
		fetcherMonitor = monitor
	}
	return memfetcher.New(ctx,
		memfetcher.WithLogLevel(util.LogLevel("fetcher")),
		memfetcher.WithMonitor(fetcherMonitor),
		memfetcher.WithStores(stores),
	)
}

func startLocker(ctx context.Context, monitor metrics.Service) (locker.Service, error) {
	var lockerMonitor metrics.LockerMonitor
	if monitor, isMonitor := monitor.(metrics.LockerMonitor); isMonitor {
		lockerMonitor = monitor
	}
	return syncmaplocker.New(ctx,
		syncmaplocker.WithLogLevel(util.LogLevel("locker")),
		syncmaplocker.WithMonitor(lockerMonitor),
	)
}

func startRuler(ctx context.Context, locker locker.Service, monitor metrics.Service) (ruler.Service, error) {
	rules, err := initRules(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to set up rules")
	}
	var rulerMonitor metrics.RulerMonitor
	if monitor, isMonitor := monitor.(metrics.RulerMonitor); isMonitor {
		rulerMonitor = monitor
	}
	return goruler.New(ctx,
		goruler.WithLogLevel(util.LogLevel("ruler")),
		goruler.WithMonitor(rulerMonitor),
		goruler.WithLocker(locker),
		goruler.WithRules(rules),
	)
}

func startPeers(ctx context.Context, monitor metrics.Service) (peers.Service, error) {
	// Keys are strings.
	peersInfo := viper.GetStringMapString("peers")
	peersMap := make(map[uint64]string)
	for k, v := range peersInfo {
		id, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse peers info")
		}
		peersMap[id] = v
	}
	var peersMonitor metrics.PeersMonitor
	if monitor, isMonitor := monitor.(metrics.PeersMonitor); isMonitor {
		peersMonitor = monitor
	}
	return staticpeers.New(ctx,
		staticpeers.WithLogLevel(util.LogLevel("peers")),
		staticpeers.WithMonitor(peersMonitor),
		staticpeers.WithPeers(peersMap),
	)
}

func startLister(ctx context.Context, monitor metrics.Service, fetcher fetcher.Service, checker checker.Service, ruler ruler.Service) (lister.Service, error) {
	var listerMonitor metrics.ListerMonitor
	if monitor, isMonitor := monitor.(metrics.ListerMonitor); isMonitor {
		listerMonitor = monitor
	}
	return standardlister.New(ctx,
		standardlister.WithLogLevel(util.LogLevel("lister")),
		standardlister.WithMonitor(listerMonitor),
		standardlister.WithFetcher(fetcher),
		standardlister.WithChecker(checker),
		standardlister.WithRuler(ruler),
	)
}

// resolvePath resolves a potentially relative path to an absolute path.
func resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	baseDir := viper.GetString("base-dir")
	if baseDir == "" {
		homeDir, err := homedir.Dir()
		if err != nil {
			log.Fatal().Err(err).Msg("Could not determine a home directory")
		}
		baseDir = homeDir
	}
	return filepath.Join(baseDir, path)
}

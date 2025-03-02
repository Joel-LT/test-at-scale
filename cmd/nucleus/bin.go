package main

// this is cmd/root_cmd.go

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LambdaTest/synapse/config"
	"github.com/LambdaTest/synapse/pkg/api"
	"github.com/LambdaTest/synapse/pkg/azure"
	"github.com/LambdaTest/synapse/pkg/cachemanager"
	"github.com/LambdaTest/synapse/pkg/command"
	"github.com/LambdaTest/synapse/pkg/core"
	"github.com/LambdaTest/synapse/pkg/diffmanager"
	"github.com/LambdaTest/synapse/pkg/gitmanager"
	"github.com/LambdaTest/synapse/pkg/global"
	"github.com/LambdaTest/synapse/pkg/lumber"
	"github.com/LambdaTest/synapse/pkg/payloadmanager"
	"github.com/LambdaTest/synapse/pkg/secret"
	"github.com/LambdaTest/synapse/pkg/server"
	"github.com/LambdaTest/synapse/pkg/service/coverage"
	"github.com/LambdaTest/synapse/pkg/service/parser"
	"github.com/LambdaTest/synapse/pkg/service/teststats"
	"github.com/LambdaTest/synapse/pkg/tasconfigmanager"
	"github.com/LambdaTest/synapse/pkg/task"
	"github.com/LambdaTest/synapse/pkg/testblocklistservice"
	"github.com/LambdaTest/synapse/pkg/testdiscoveryservice"
	"github.com/LambdaTest/synapse/pkg/testexecutionservice"
	"github.com/LambdaTest/synapse/pkg/zstd"
	"github.com/spf13/cobra"
)

// RootCommand will setup and return the root command
func RootCommand() *cobra.Command {
	rootCmd := cobra.Command{
		Use:     "nucleus",
		Long:    `nucleus is a coordinator binary used as entrypoint in tas containers`,
		Version: global.NUCLEUS_BINARY_VERSION,
		Run:     run,
	}

	// define flags used for this command
	AttachCLIFlags(&rootCmd)

	return &rootCmd
}

func run(cmd *cobra.Command, args []string) {
	// create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// timeout in seconds
	const gracefulTimeout = 5000 * time.Millisecond

	// a WaitGroup for the goroutines to tell us they've stopped
	wg := sync.WaitGroup{}

	cfg, err := config.LoadNucleusConfig(cmd)
	if err != nil {
		fmt.Printf("[Error] Failed to load config: " + err.Error())
		os.Exit(1)
	}

	// patch logconfig file location with root level log file location
	if cfg.LogFile != "" {
		cfg.LogConfig.FileLocation = filepath.Join(cfg.LogFile, "nucleus.log")
	}

	// You can also use logrus implementation
	// by using lumber.InstanceLogrusLogger
	logger, err := lumber.NewLogger(cfg.LogConfig, cfg.Verbose, lumber.InstanceZapLogger)
	if err != nil {
		log.Fatalf("Could not instantiate logger %s", err.Error())
	}
	logger.Debugf("Running on local: %t", cfg.LocalRunner)

	if cfg.LocalRunner {
		logger.Infof("Local runner detected , changing IP from: %s to: %s", global.NeuronHost, cfg.SynapseHost)
		global.SetNeuronHost(strings.TrimSpace(cfg.SynapseHost))

		logger.Infof("change neuron host to %s", global.NeuronHost)
	} else {
		global.SetNeuronHost(global.NeuronRemoteHost)
	}
	pl, err := core.NewPipeline(cfg, logger)
	if err != nil {
		logger.Errorf("Unable to create the pipeline: %+v\n", err)
		logger.Errorf("Aborting ...")
		os.Exit(1)
	}

	ts, err := teststats.New(cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize test stats service: %v", err)
	}
	azureClient, err := azure.NewAzureBlobEnv(cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize azure blob: %v", err)
	}
	if err != nil && !cfg.LocalRunner {
		logger.Fatalf("failed to initialize azure blob: %v", err)
	}

	// attach plugins to pipeline
	pm := payloadmanager.NewPayloadManger(azureClient, logger, cfg)
	secretParser := secret.New(logger)
	tcm := tasconfigmanager.NewTASConfigManager(logger)
	gm := gitmanager.NewGitManager(logger)
	dm := diffmanager.NewDiffManager(cfg, logger)
	execManager := command.NewExecutionManager(secretParser, azureClient, logger)
	tds := testdiscoveryservice.NewTestDiscoveryService(execManager, logger)
	tes := testexecutionservice.NewTestExecutionService(execManager, azureClient, ts, logger)
	tbs, err := testblocklistservice.NewTestBlockListService(cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize test blocklist service: %v", err)
	}
	router := api.NewRouter(logger, ts)

	t, err := task.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize task: %v", err)
	}

	zstd, err := zstd.New(execManager, logger)
	if err != nil {
		logger.Fatalf("failed to initialize zstd compressor: %v", err)
	}
	cache, err := cachemanager.New(zstd, azureClient, logger)
	if err != nil {
		logger.Fatalf("failed to initialize cache manager: %v", err)
	}

	parserService, err := parser.New(ctx, tcm, logger)
	if err != nil {
		logger.Fatalf("failed to initialize parser service: %v", err)
	}
	coverageService, err := coverage.New(execManager, azureClient, zstd, cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize coverage service: %v", err)
	}

	pl.PayloadManager = pm
	pl.TASConfigManager = tcm
	pl.GitManager = gm
	pl.DiffManager = dm
	pl.TestDiscoveryService = tds
	pl.TestBlockListService = tbs
	pl.TestExecutionService = tes
	pl.ExecutionManager = execManager
	pl.ParserService = parserService
	pl.CoverageService = coverageService
	pl.TestStats = ts
	pl.Task = t
	pl.CacheStore = cache
	pl.SecretParser = secretParser

	logger.Infof("LambdaTest Nucleus version: %s", global.NUCLEUS_BINARY_VERSION)

	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()
		// starting pipeline
		pl.Start(ctx)
	}()
	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()
		server.ListenAndServe(ctx, router, cfg, logger)
	}()
	// listen for C-c
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// create channel to mark status of waitgroup
	// this is required to brutally kill application in case of
	// timeout
	done := make(chan struct{})

	// asynchronously wait for all the go routines
	go func() {
		// and wait for all go routines
		wg.Wait()
		logger.Debugf("main: all goroutines have finished.")
		close(done)
	}()

	// wait for signal channel
	select {
	case <-c:
		{
			logger.Debugf("main: received C-c - attempting graceful shutdown ....")
			// tell the goroutines to stop
			logger.Debugf("main: telling goroutines to stop")
			cancel()
			select {
			case <-done:
				logger.Debugf("Go routines exited within timeout")
			case <-time.After(gracefulTimeout):
				logger.Errorf("Graceful timeout exceeded. Brutally killing the application")
			}

		}
	case <-done:
		os.Exit(0)
	}

}

package main

import (
	"github.com/apcera/nats"
	cf_debug_server "github.com/cloudfoundry-incubator/cf-debug-server"
	"github.com/cloudfoundry/dropsonde"
	"github.com/cloudfoundry/gorouter/access_log"
	vcap "github.com/cloudfoundry/gorouter/common"
	"github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/proxy"
	rregistry "github.com/cloudfoundry/gorouter/registry"
	"github.com/cloudfoundry/gorouter/router"
	rvarz "github.com/cloudfoundry/gorouter/varz"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats"

	"flag"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

var configFile string

func init() {
	flag.StringVar(&configFile, "c", "", "Configuration File")

	flag.Parse()
}

func main() {
	c := config.DefaultConfig()
	logCounter := vcap.NewLogCounter()
	InitLoggerFromConfig(c, logCounter)
	logger := steno.NewLogger("router.main")

	if configFile != "" {
		c = config.InitConfigFromFile(configFile)
	}

	err := dropsonde.Initialize(c.Logging.MetronAddress, c.Logging.JobName)
	if err != nil {
		logger.Errorf("Dropsonde failed to initialize: %s", err.Error())
		os.Exit(1)
	}

	// setup number of procs
	if c.GoMaxProcs != 0 {
		runtime.GOMAXPROCS(c.GoMaxProcs)
	}

	if c.DebugAddr != "" {
		cf_debug_server.SetAddr(c.DebugAddr)
		cf_debug_server.Run()
	}

	natsServers := c.NatsServers()
	natsClient, err := yagnats.Connect(natsServers)
	if err != nil {
		logger.Fatalf("Error connecting to NATS: %s\n", err)
	}

	natsClient.AddClosedCB(func(conn *nats.Conn) {
		logger.Errorf("Close on NATS client. nats.Conn: %+v", *conn)
		os.Exit(1)
	})

	registry := rregistry.NewRouteRegistry(c, natsClient)

	varz := rvarz.NewVarz(registry)

	accessLogger, err := access_log.CreateRunningAccessLogger(c)
	if err != nil {
		logger.Fatalf("Error creating access logger: %s\n", err)
	}

	args := proxy.ProxyArgs{
		EndpointTimeout: c.EndpointTimeout,
		Ip:              c.Ip,
		TraceKey:        c.TraceKey,
		Registry:        registry,
		Reporter:        varz,
		AccessLogger:    accessLogger,
	}
	p := proxy.NewProxy(args)

	router, err := router.NewRouter(c, p, natsClient, registry, varz, logCounter)
	if err != nil {
		logger.Errorf("An error occurred: %s", err.Error())
		os.Exit(1)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	errChan := router.Run()

	logger.Info("gorouter.started")

	select {
	case err := <-errChan:
		if err != nil {
			logger.Errorf("Error occurred: %s", err.Error())
			os.Exit(1)
		}
	case sig := <-signals:
		go func() {
			for sig := range signals {
				logger.Infod(
					map[string]interface{}{
						"signal": sig.String(),
					},
					"gorouter.signal.ignored",
				)
			}
		}()

		if sig == syscall.SIGUSR1 {
			logger.Infod(
				map[string]interface{}{
					"timeout": (c.DrainTimeout).String(),
				},
				"gorouter.draining",
			)

			router.Drain(c.DrainTimeout)
		}

		stoppingAt := time.Now()

		logger.Info("gorouter.stopping")

		router.Stop()

		logger.Infod(
			map[string]interface{}{
				"took": time.Since(stoppingAt).String(),
			},
			"gorouter.stopped",
		)
	}

	os.Exit(0)
}

func InitLoggerFromConfig(c *config.Config, logCounter *vcap.LogCounter) {
	l, err := steno.GetLogLevel(c.Logging.Level)
	if err != nil {
		panic(err)
	}

	s := make([]steno.Sink, 0, 3)
	if c.Logging.File != "" {
		s = append(s, steno.NewFileSink(c.Logging.File))
	} else {
		s = append(s, steno.NewIOSink(os.Stdout))
	}

	if c.Logging.Syslog != "" {
		s = append(s, steno.NewSyslogSink(c.Logging.Syslog))
	}

	s = append(s, logCounter)

	stenoConfig := &steno.Config{
		Sinks: s,
		Codec: steno.NewJsonCodec(),
		Level: l,
	}

	steno.Init(stenoConfig)
}

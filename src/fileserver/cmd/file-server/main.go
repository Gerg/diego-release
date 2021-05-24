package main

import (
	"crypto/tls"
	"flag"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/debugserver"
	loggingclient "code.cloudfoundry.org/diego-logging-client"
	"code.cloudfoundry.org/diego-release/fileserver/cmd/file-server/config"
	"code.cloudfoundry.org/diego-release/fileserver/handlers"
	"code.cloudfoundry.org/go-loggregator/runtimeemitter"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/diego-release/locket"
	"code.cloudfoundry.org/tlsconfig"
	"github.com/hashicorp/consul/api"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
)

var configFilePath = flag.String(
	"config",
	"",
	"The path to the JSON configuration file.",
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()
	cfg, err := config.NewFileServerConfig(*configFilePath)
	if err != nil {
		logger, _ := lagerflags.NewFromConfig("file-server", lagerflags.DefaultLagerConfig())
		logger.Fatal("failed-to-parse-config", err)
	}

	logger, reconfigurableSink := lagerflags.NewFromConfig("file-server", cfg.LagerConfig)

	_, err = initializeMetron(logger, cfg)
	if err != nil {
		logger.Error("failed-to-initialize-metron-client", err)
		os.Exit(1)
	}

	consulClient, err := consuladapter.NewClientFromUrl(cfg.ConsulCluster)
	if err != nil {
		logger.Fatal("new-client-failed", err)
	}

	var tlsConfig *tls.Config
	if cfg.HTTPSServerEnabled {
		if len(cfg.HTTPSListenAddr) == 0 {
			logger.Fatal("invalid-https-configuration", nil)
		}
		var err error
		tlsConfig, err = tlsconfig.Build(
			tlsconfig.WithInternalServiceDefaults(),
			tlsconfig.WithIdentityFromFile(cfg.CertFile, cfg.KeyFile),
		).Server()
		if err != nil {
			logger.Fatal("failed-to-create-tls-config", err)
		}
	}
	members := grouper.Members{
		{"file server", initializeServer(logger, cfg.StaticDirectory, cfg.ServerAddress, cfg.HTTPSListenAddr, tlsConfig)},
	}

	if cfg.EnableConsulServiceRegistration {
		registrationRunner := initializeRegistrationRunner(logger, consulClient, cfg.ServerAddress, clock.NewClock())
		members = append(members, grouper.Member{"registration-runner", registrationRunner})
	}

	if dbgAddr := debugserver.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", debugserver.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group))
	logger.Info("ready")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initializeMetron(logger lager.Logger, config config.FileServerConfig) (loggingclient.IngressClient, error) {
	client, err := loggingclient.NewIngressClient(config.LoggregatorConfig)
	if err != nil {
		return nil, err
	}

	if config.LoggregatorConfig.UseV2API {
		emitter := runtimeemitter.NewV1(client)
		go emitter.Run()
	}

	return client, nil
}

func initializeServer(logger lager.Logger, staticDirectory, serverAddress, serverAddressTls string, tlsConfig *tls.Config) ifrit.Runner {
	if staticDirectory == "" {
		logger.Fatal("static-directory-missing", nil)
	}

	fileServerHandler, err := handlers.New(staticDirectory, logger)
	if err != nil {
		logger.Error("router-building-failed", err)
		os.Exit(1)
	}

	if tlsConfig != nil {
		return grouper.NewParallel(os.Interrupt, grouper.Members{
			{Name: "tls-server", Runner: http_server.NewTLSServer(serverAddressTls, fileServerHandler, tlsConfig)},
			{
				Name: "redirect-server",
				Runner: http_server.New(serverAddress, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					httpHostPort := strings.Split(r.Host, ":")
					tlsHostPort := strings.Split(serverAddressTls, ":")
					httpsHost := httpHostPort[0] + ":" + tlsHostPort[1]
					http.Redirect(w, r, "https://"+httpsHost+r.URL.String(), http.StatusMovedPermanently)
				})),
			},
		})
	}

	return http_server.New(serverAddress, fileServerHandler)
}

func initializeRegistrationRunner(logger lager.Logger, consulClient consuladapter.Client, listenAddress string, clock clock.Clock) ifrit.Runner {
	_, portString, err := net.SplitHostPort(listenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}
	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	registration := &api.AgentServiceRegistration{
		Name: "file-server",
		Port: portNum,
		Check: &api.AgentServiceCheck{
			TTL: "20s",
		},
	}

	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}

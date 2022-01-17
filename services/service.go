package services

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/forta-protocol/forta-node/ens"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/forta-protocol/forta-node/config"
)

type Service interface {
	Start() error
	Stop() error
	Name() string
}

var processGrp *errgroup.Group
var sigc chan os.Signal

var execIDKey = struct{}{}

func ExecID(ctx context.Context) string {
	execID := ctx.Value(execIDKey)
	if execID == nil {
		panic("cannot get exec ID")
	}
	return execID.(string)
}

func initExecID(ctx context.Context) context.Context {
	execID, err := uuid.NewUUID()
	if err != nil {
		panic(err)
	}
	return context.WithValue(ctx, execIDKey, execID.String())
}

func setContracts(cfg *config.Config) error {
	contracts, err := ens.ResolveFortaContracts(cfg.ENSConfig.JsonRpc.Url, cfg.ENSConfig.ContractAddress)
	if err != nil {
		return err
	}
	if cfg.Registry.ContractAddress == "" {
		cfg.Registry.ContractAddress = contracts.Dispatch
	}
	cfg.ScannerVersionContractAddress = contracts.ScannerVersion
	cfg.AgentRegistryContractAddress = contracts.Agent
	return nil
}

func ContainerMain(name string, getServices func(ctx context.Context, cfg config.Config) ([]Service, error)) {
	cfg, err := config.GetConfigForContainer()
	if err != nil {
		log.WithError(err).Errorf("could not get config for container '%s'", name)
		return
	}

	if err := setContracts(&cfg); err != nil {
		log.WithError(err).Error("could not initialize contracts for config")
	}

	lvl, err := log.ParseLevel(cfg.Log.Level)
	if err != nil {
		log.WithError(err).Error("could not initialize log level")
		return
	}
	log.SetLevel(lvl)
	log.Infof("Starting %s", name)

	ctx, cancel := InitMainContext()
	defer cancel()

	serviceList, err := getServices(ctx, cfg)
	if err != nil {
		log.WithError(err).Error("could not initialize services")
		return
	}

	if err := StartServices(ctx, cancel, serviceList); err != nil {
		log.Error("error running services: ", err)
	}

	log.Infof("Stopping %s", name)
}

func InitMainContext() (context.Context, context.CancelFunc) {
	execIDCtx := initExecID(context.Background())
	cCtx, cancel := context.WithCancel(execIDCtx)
	grp, ctx := errgroup.WithContext(cCtx)
	processGrp = grp
	if sigc == nil {
		sigc = make(chan os.Signal, 1)
	}
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		sig := <-sigc
		log.Infof("received signal: %s", sig.String())
		cancel()
	}()
	return ctx, cancel
}

// StartServices kicks off all services and blocks until an error is returned or context ends
func StartServices(ctx context.Context, cancelParent context.CancelFunc, services []Service) error {
	if processGrp == nil {
		panic("InitMainContext must be called first")
	}

	// wait for context to stop (service.Start may either block or be async)
	processGrp.Go(func() error {
		select {
		case <-ctx.Done():
			log.WithError(ctx.Err()).Info("context is done")
			return ctx.Err()
		}
	})

	// each service should be able to start successfully within reasonable time
	for _, service := range services {
		serviceStartedCtx, serviceStarted := context.WithCancel(context.Background())
		defer serviceStarted()

		logger := log.WithField("service", service.Name())

		go func() {
			if err := service.Start(); err != nil {
				logger.WithError(err).Error("failed to start service")
				cancelParent()
				return
			}
			serviceStarted()
		}()

		select {
		case <-time.After(time.Minute):
			cancelParent()
			break
		case <-serviceStartedCtx.Done():
			// clean up each service
			defer func() {
				if err := service.Stop(); err != nil {
					logger.WithError(err).Error("error while stopping")
				}
			}()
		}
	}

	log.Info("grp.Wait()...")
	err := processGrp.Wait()
	if err != nil {
		log.WithError(err).Error("StartServices ending with errgroup err")
	}
	return err
}

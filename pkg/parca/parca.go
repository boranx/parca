package parca

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/oklog/run"
	"github.com/parca-dev/parca/pkg/debuginfo"
	"github.com/parca-dev/parca/pkg/profilestore"
	"github.com/parca-dev/parca/pkg/query"
	"github.com/parca-dev/parca/pkg/server"
	"github.com/parca-dev/parca/pkg/storage"
	debuginfopb "github.com/parca-dev/parca/proto/gen/go/debuginfo"
	profilestorepb "github.com/parca-dev/parca/proto/gen/go/profilestore"
	querypb "github.com/parca-dev/parca/proto/gen/go/query"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
)

type Flags struct {
	ConfigPath         string   `kong:"help='Path to config file.',default='parca.yaml'"`
	LogLevel           string   `kong:"enum='error,warn,info,debug',help='Log level.',default='info'"`
	Port               string   `kong:"help='Port string for server',default=':7070'"`
	CORSAllowedOrigins []string `kong:"help='Allowed CORS origins.'"`
}

// Config is the configuration for debug info storage
type Config struct {
	DebugInfo *debuginfo.Config `yaml:"debug_info"`
}

// Run the parca server
func Run(ctx context.Context, logger log.Logger, flags *Flags) error {
	cfgContent, err := ioutil.ReadFile(flags.ConfigPath)
	if err != nil {
		level.Error(logger).Log("msg", "failed to read config", "path", flags.ConfigPath)
		return err
	}

	cfg := Config{}
	if err := yaml.Unmarshal(cfgContent, &cfg); err != nil {
		level.Error(logger).Log("msg", "failed to parse config", "err", err, "path", flags.ConfigPath)
		return err
	}

	d, err := debuginfo.NewStore(logger, cfg.DebugInfo)
	if err != nil {
		level.Error(logger).Log("msg", "failed to initialize debug info store", "err", err)
		return err
	}

	db := storage.OpenDB()
	metaStore := storage.NewInMemoryProfileMetaStore()
	s := profilestore.NewProfileStore(logger, db, metaStore)
	q := query.New(logger, db, metaStore)

	parcaserver := &server.Server{}

	var gr run.Group
	gr.Add(run.SignalHandler(ctx, os.Interrupt, syscall.SIGINT, syscall.SIGTERM))
	gr.Add(
		func() error {
			return parcaserver.ListenAndServe(
				ctx,
				logger,
				flags.Port,
				flags.CORSAllowedOrigins,
				server.RegisterableFunc(func(ctx context.Context, srv *grpc.Server, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
					debuginfopb.RegisterDebugInfoServer(srv, d)
					profilestorepb.RegisterProfileStoreServer(srv, s)
					querypb.RegisterQueryServer(srv, q)

					if err := debuginfopb.RegisterDebugInfoHandlerFromEndpoint(ctx, mux, endpoint, opts); err != nil {
						return err
					}

					if err := profilestorepb.RegisterProfileStoreHandlerFromEndpoint(ctx, mux, endpoint, opts); err != nil {
						return err
					}

					if err := querypb.RegisterQueryHandlerFromEndpoint(ctx, mux, endpoint, opts); err != nil {
						return err
					}

					return nil
				}),
			)
		},
		func(_ error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // TODO make this a graceful shutdown config setting
			defer cancel()

			err := parcaserver.Shutdown(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				level.Error(logger).Log("msg", "error shuttiing down server", "err", err)
			}
		},
	)

	if err := gr.Run(); err != nil {
		if _, ok := err.(run.SignalError); ok {
			return nil
		}
		return err
	}

	return nil
}
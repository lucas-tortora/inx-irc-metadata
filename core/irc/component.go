package irc

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"go.uber.org/dig"

	"github.com/iotaledger/hive.go/core/app"
	"github.com/iotaledger/inx-app/pkg/httpserver"
	"github.com/iotaledger/inx-app/pkg/nodebridge"
	"github.com/iotaledger/inx-irc-metadata/pkg/daemon"
	iotago "github.com/iotaledger/iota.go/v3"
	"github.com/iotaledger/iota.go/v3/nodeclient"
)

const (
	IRC27SchemaURL = "https://raw.githubusercontent.com/iotaledger/tips/main/tips/TIP-0027/irc27.schema.json"
	IRC30SchemaURL = "https://raw.githubusercontent.com/iotaledger/tips/main/tips/TIP-0030/irc30.schema.json"
)

func init() {
	CoreComponent = &app.CoreComponent{
		Component: &app.Component{
			Name:     "IRC",
			Params:   params,
			DepsFunc: func(cDeps dependencies) { deps = cDeps },
			Provide:  provide,
			Run:      run,
		},
	}
}

var (
	CoreComponent *app.CoreComponent
	deps          dependencies
)

type dependencies struct {
	dig.In
	NodeBridge *nodebridge.NodeBridge

	IRC27Validator *MetadataValidator[iotago.NFTID]
	IRC30Validator *MetadataValidator[iotago.FoundryID]
}

func provide(c *dig.Container) error {
	type inDeps struct {
		dig.In
		NodeBridge *nodebridge.NodeBridge
	}

	type outDeps struct {
		dig.Out
		IRC27Validator *MetadataValidator[iotago.NFTID]
		IRC30Validator *MetadataValidator[iotago.FoundryID]
	}

	return c.Provide(func(deps inDeps) outDeps {
		client := deps.NodeBridge.INXNodeClient()

		indexer, err := client.Indexer(context.Background())
		if err != nil {
			panic(err)
		}

		irc27, err := NewMetadataValidator[iotago.NFTID](IRC27SchemaURL, ParamsRestAPI.MetadataCacheSize,
			func(c echo.Context) (iotago.NFTID, error) {
				nftID, err := httpserver.ParseNFTIDParam(c, ParameterNFTID)
				if err != nil {
					return iotago.NFTID{}, err
				}
				return *nftID, nil
			},
			func(ctx context.Context, key iotago.NFTID) ([]byte, error) {
				_, output, _, err := indexer.NFT(ctx, key)
				if err != nil {
					if errors.Is(err, nodeclient.ErrIndexerNotFound) {
						return nil, echo.ErrNotFound
					}
					return nil, err
				}

				features, err := output.ImmutableFeatures.Set()
				if err != nil {
					return nil, httpserver.ErrNotAcceptable
				}

				metadata := features.MetadataFeature()
				if metadata == nil {
					return nil, httpserver.ErrNotAcceptable
				}

				return metadata.Data, nil
			})
		if err != nil {
			panic(err)
		}

		irc30, err := NewMetadataValidator[iotago.FoundryID](IRC30SchemaURL, ParamsRestAPI.MetadataCacheSize,
			func(c echo.Context) (iotago.FoundryID, error) {
				foundryID, err := httpserver.ParseFoundryIDParam(c, ParameterNativeTokenID)
				if err != nil {
					return iotago.FoundryID{}, err
				}
				return *foundryID, nil
			},
			func(ctx context.Context, key iotago.FoundryID) ([]byte, error) {
				_, output, _, err := indexer.Foundry(ctx, key)
				if err != nil {
					if errors.Is(err, nodeclient.ErrHTTPNotFound) {
						return nil, ErrLoadMetadataNotFound
					}
					return nil, err
				}

				features, err := output.ImmutableFeatures.Set()
				if err != nil {
					return nil, ErrLoadMetadataInvalid
				}

				metadata := features.MetadataFeature()
				if metadata == nil {
					return nil, ErrLoadMetadataInvalid
				}

				return metadata.Data, nil
			})
		if err != nil {
			panic(err)
		}

		return outDeps{
			IRC27Validator: irc27,
			IRC30Validator: irc30,
		}
	})

}

func run() error {
	// create a background worker that handles the API
	if err := CoreComponent.Daemon().BackgroundWorker("API", func(ctx context.Context) {
		CoreComponent.LogInfo("Starting API ... done")

		e := httpserver.NewEcho(CoreComponent.Logger(), nil, ParamsRestAPI.DebugRequestLoggerEnabled)

		CoreComponent.LogInfo("Starting API server ...")

		setupRoutes(e)
		go func() {
			CoreComponent.LogInfof("You can now access the API using: http://%s", ParamsRestAPI.BindAddress)
			if err := e.Start(ParamsRestAPI.BindAddress); err != nil && !errors.Is(err, http.ErrServerClosed) {
				CoreComponent.LogErrorfAndExit("Stopped REST-API server due to an error (%s)", err)
			}
		}()

		ctxRegister, cancelRegister := context.WithTimeout(ctx, 5*time.Second)

		advertisedAddress := ParamsRestAPI.BindAddress
		if ParamsRestAPI.AdvertiseAddress != "" {
			advertisedAddress = ParamsRestAPI.AdvertiseAddress
		}

		if err := deps.NodeBridge.RegisterAPIRoute(ctxRegister, APIRoute, advertisedAddress); err != nil {
			CoreComponent.LogErrorfAndExit("Registering INX api route failed: %s", err)
		}
		cancelRegister()

		CoreComponent.LogInfo("Starting API server ... done")
		<-ctx.Done()
		CoreComponent.LogInfo("Stopping API ...")

		ctxUnregister, cancelUnregister := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUnregister()

		//nolint:contextcheck // false positive
		if err := deps.NodeBridge.UnregisterAPIRoute(ctxUnregister, APIRoute); err != nil {
			CoreComponent.LogWarnf("Unregistering INX api route failed: %s", err)
		}

		shutdownCtx, shutdownCtxCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCtxCancel()

		//nolint:contextcheck // false positive
		if err := e.Shutdown(shutdownCtx); err != nil {
			CoreComponent.LogWarn(err)
		}

		CoreComponent.LogInfo("Stopping API ... done")
	}, daemon.PriorityStopRestAPI); err != nil {
		CoreComponent.LogPanicf("failed to start worker: %s", err)
	}

	return nil
}

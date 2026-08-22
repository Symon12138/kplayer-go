package server

import (
	"context"
	"fmt"
	"github.com/bytelang/kplayer/cmd"
	"github.com/bytelang/kplayer/engine"
	"github.com/bytelang/kplayer/management"
	"github.com/bytelang/kplayer/module"
	outputprovider "github.com/bytelang/kplayer/module/output/provider"
	playprovider "github.com/bytelang/kplayer/module/play/provider"
	pluginprovider "github.com/bytelang/kplayer/module/plugin/provider"
	resourceprovider "github.com/bytelang/kplayer/module/resource/provider"
	kptypes "github.com/bytelang/kplayer/types"
	autherror "github.com/bytelang/kplayer/types/error"
	"github.com/bytelang/kplayer/types/server"
	"github.com/bytelang/kplayer/webconsole"
	"github.com/gorilla/websocket"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"net"
	"net/http"
	"strings"
)

type httpServer struct {
	authOn    bool
	authToken string
}

func NewHttpServer() *httpServer {
	return &httpServer{}
}

var _ server.ServerCreator = &httpServer{}

// apiRouter dispatches between the management REST API, the embedded
// operations console and the grpc-gateway mux so that all of them share the
// single HTTP listener. Existing grpc-gateway and WebSocket routes are
// forwarded unchanged to gateway.
type apiRouter struct {
	gateway    http.Handler
	management http.Handler
	console    http.Handler
}

func (r *apiRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	if r.console != nil && (p == webconsole.MountPrefix || strings.HasPrefix(p, webconsole.MountPrefix+"/")) {
		r.console.ServeHTTP(w, req)
		return
	}
	if r.management != nil && isManagementPath(p) {
		r.management.ServeHTTP(w, req)
		return
	}
	r.gateway.ServeHTTP(w, req)
}

// isManagementPath reports whether a request path belongs to the management
// REST API. It must stay disjoint from the grpc-gateway module routes
// (/play, /resource, /output, /plugin) and /v1/operations/{name}.
func isManagementPath(p string) bool {
	for _, root := range []string{
		"/status", "/auth", "/user", "/media", "/playlist", "/task", "/alarm", "/scheduler", "/player", "/output-group",
		"/failover", "/health-policy", "/cache-task", "/scene-template", "/webhook", "/audit",
		"/node", "/instance", "/remote-command", "/config-snapshot", "/config-template",
		"/industry-template", "/smart-rule", "/suggestion", "/metrics", "/engine",
		"/stream", "/effects",
	} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

type Validator interface {
	Validate() error
}

// authAllowed reports whether an incoming request carrying gotToken is
// authorised. With authentication disabled every request passes, so a
// non-empty configured token never forces a metadata requirement on its own.
// When authentication is enabled the request must carry a token exactly equal
// to the configured token; an empty configured token therefore never
// authorises anything (fail closed), so turning auth on can never silently
// leave the door open.
func authAllowed(authOn bool, authToken, gotToken string) bool {
	if !authOn {
		return true
	}
	return authToken != "" && gotToken == authToken
}

// incomingToken extracts the first authorization token from the gRPC incoming
// metadata, returning "" when the metadata is absent or carries no value. It
// never panics on a missing or empty metadata map.
func incomingToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if md == nil {
		return ""
	}
	tokens := md.Get(server.AUTHORIZATION_METADATA_KEY)
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func (h *httpServer) StartServer(stopChan chan bool, mm module.ModuleManager, authOn bool, authToken string) {
	h.authToken = authToken
	h.authOn = authOn

	// modules
	playModule := mm.GetModule(playprovider.ModuleName).(playprovider.ProviderI)
	outputModule := mm.GetModule(outputprovider.ModuleName).(outputprovider.ProviderI)
	//pluginModule := mm.GetModule(pluginprovider.ModuleName).(pluginprovider.ProviderI)
	resourceModule := mm.GetModule(resourceprovider.ModuleName).(resourceprovider.ProviderI)

	grpcEndpoint := fmt.Sprintf("%s:%d", playModule.GetRPCParams().Address, playModule.GetRPCParams().GrpcPort)
	httpEndpoint := fmt.Sprintf("%s:%d", playModule.GetRPCParams().Address, playModule.GetRPCParams().HttpPort)

	// The ffmpeg engine replaces the stub playback path: it decodes,
	// encodes and pushes through real ffmpeg subprocesses, with its
	// configuration in engine.json in the working directory. A missing
	// file yields the default configuration (no outputs: playback is
	// rejected until outputs are configured via POST /engine/ffmpeg); a
	// corrupt file disables the engine and keeps the legacy stub path.
	var eng engine.Engine
	// The global effect chain (watermarks / subtitles / marquee / ...) is
	// merged into every engine's output filter graph at construction time:
	// rendered -vf/-af strings become the base, per-output filters append.
	// One EffectManager instance is shared by the main engine, the handler
	// and the stream manager so all three observe the same list.
	effMgr := NewEffectManager(effectFile)
	if cfg, err := engine.Load(); err != nil {
		log.WithField("error", err).Error("load engine config failed; engine disabled")
	} else {
		if vf, af, err := effMgr.Render(); err == nil {
			cfg.Outputs = mergeEffectFilters(cfg.Outputs, vf, af)
		}
		eng = engine.NewFFmpegEngine(cfg)
	}

	// The management REST API owns a local Store and the scheduler runtime.
	// It exposes the scheduler lifecycle (/scheduler/start, /scheduler/stop)
	// alongside the media/playlist/task/alarm endpoints. If it cannot be
	// initialised the HTTP server still serves the grpc-gateway and console.
	var managementAPI http.Handler
	var scheduler *management.Scheduler
	var failoverMonitor *management.FailoverMonitor
	if mgmt, err := newManagementHandlerWithEngine(playModule, resourceModule, outputModule, h.authOn, h.authToken, eng, effMgr); err != nil {
		log.WithField("error", err).Error("initialize management api failed; management routes disabled")
	} else {
		managementAPI = mgmt
		scheduler = mgmt.scheduler
		// First-phase default scheduling: start the scheduler as soon as the
		// management API is ready so persisted tasks begin running. It can be
		// paused manually via /scheduler/stop and resumed via /scheduler/start.
		if err := scheduler.Start(); err != nil {
			log.WithField("error", err).Error("start scheduler failed; use /scheduler/start to start it")
		}
		// The failover monitor follows the same lifecycle: it starts with the
		// server so enabled failovers are evaluated from the first tick. A
		// failed start only disables failover switching, not the REST API.
		failoverMonitor = mgmt.failoverMonitor
		if err := failoverMonitor.Start(); err != nil {
			log.WithField("error", err).Error("start failover monitor failed")
		}
	}

	opts := []grpc_recovery.Option{
		grpc_recovery.WithRecoveryHandler(func(p interface{}) (err error) {
			return status.Errorf(codes.Unknown, "panic triggered: %v", p)
		}),
	}
	reqValidatorInterceptor := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		if p, ok := req.(Validator); ok {
			if err := p.Validate(); err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		}

		if !authAllowed(h.authOn, h.authToken, incomingToken(ctx)) {
			return nil, status.Error(codes.Unauthenticated, autherror.AuthTokenInvalid.Error())
		}
		return handler(ctx, req)
	}

	grpcSvc := grpc.NewServer(
		grpc_middleware.WithUnaryServerChain(
			grpc_recovery.UnaryServerInterceptor(opts...),
			grpc_middleware.ChainUnaryServer(reqValidatorInterceptor),
		),
		grpc_middleware.WithStreamServerChain(grpc_recovery.StreamServerInterceptor(opts...)),
	)
	httpSvc := http.Server{}

	go func() {
		// grpc server
		listen, err := net.Listen("tcp", grpcEndpoint)
		if err != nil {
			log.WithField("error", err).Fatal("start grpc gateway server failed")
		}

		playServer := mm.GetModule(playprovider.ModuleName).(server.PlayGreeterServer)
		outputServer := mm.GetModule(outputprovider.ModuleName).(server.OutputGreeterServer)
		pluginServer := mm.GetModule(pluginprovider.ModuleName).(server.PluginGreeterServer)
		resourceServer := mm.GetModule(resourceprovider.ModuleName).(server.ResourceGreeterServer)

		server.RegisterPlayGreeterServer(grpcSvc, playServer)
		server.RegisterOutputGreeterServer(grpcSvc, outputServer)
		server.RegisterPluginGreeterServer(grpcSvc, pluginServer)
		server.RegisterResourceGreeterServer(grpcSvc, resourceServer)

		err = grpcSvc.Serve(listen)
		if err != nil && err != grpc.ErrServerStopped {
			log.WithField("error", err).Fatal("start grpc gateway server failed")
		}
		log.Info("rpc server shutdown success")
	}()

	go func() {
		// grpc-gateway server
		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// http server
		listen, err := net.Listen("tcp", httpEndpoint)
		if err != nil {
			log.WithField("error", err).Fatal("start grpc gateway server failed")
		}

		// Register gRPC server endpoint
		mux := runtime.NewServeMux(
			runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.HTTPBodyMarshaler{Marshaler: &runtime.JSONPb{
				MarshalOptions: protojson.MarshalOptions{
					UseProtoNames:   true,
					EmitUnpopulated: true,
					UseEnumNumbers:  false,
				},
				UnmarshalOptions: protojson.UnmarshalOptions{
					DiscardUnknown: true,
				},
			}}),
			runtime.WithErrorHandler(protoErrorHandle),
			runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
				switch key {
				case server.AUTHORIZATION_METADATA_KEY:
					return key, true
				}
				return "", false
			}),
		)

		// inject websocket
		var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			return true
		}}
		mux.HandlePath("GET", "/websocket", func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				log.WithFields(log.Fields{"error": err, "address": r.RemoteAddr}).Error("can not connected websocket client")
				w.Write([]byte(err.Error()))
				return
			}
			defer conn.Close()

			log.WithField("address", r.RemoteAddr).Debug("success connected websocket client")

			// validate auth token
			if !authAllowed(h.authOn, h.authToken, r.Header.Get(server.AUTHORIZATION_METADATA_KEY)) {
				conn.WriteMessage(websocket.TextMessage, []byte("Connection forbidden. auth token invalid"))
				return
			}

			// subscribe message
			websocketName := "websocket-" + conn.RemoteAddr().String()
			sub, err := cmd.SubscribeMessage(websocketName)
			if err != nil {
				log.WithFields(log.Fields{"error": err, "address": conn.RemoteAddr()}).Error("subscribe message failed")
			}
			defer cmd.CancelSubscribeMessage(websocketName)
			for {
				message, ok := <-sub
				if !ok {
					// The subscription channel was closed (e.g. during shutdown);
					// stop reading so the client connection can be released.
					log.WithField("address", r.RemoteAddr).Debug("websocket subscription channel closed; disconnecting client")
					break
				}
				jsonRawMessage, err := kptypes.ParseMessageToJson(&message)
				if err != nil {
					log.WithFields(log.Fields{"error": err, "message": message}).Fatal("message cannot encode to json")
					break
				}

				err = conn.WriteMessage(websocket.TextMessage, []byte(jsonRawMessage))
				if err != nil {
					log.WithField("error", err).Debug("send websocket client failed")
					break
				}
			}
		})
		opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		err = server.RegisterPlayGreeterHandlerFromEndpoint(ctx, mux, grpcEndpoint, opts)
		if err != nil {
			log.WithField("error", err).Panic("register grpc gateway server failed")
		}
		err = server.RegisterOutputGreeterHandlerFromEndpoint(ctx, mux, grpcEndpoint, opts)
		if err != nil {
			log.WithField("error", err).Panic("register grpc gateway server failed")
		}
		err = server.RegisterPluginGreeterHandlerFromEndpoint(ctx, mux, grpcEndpoint, opts)
		if err != nil {
			log.WithField("error", err).Panic("register grpc gateway server failed")
		}
		err = server.RegisterResourceGreeterHandlerFromEndpoint(ctx, mux, grpcEndpoint, opts)
		if err != nil {
			log.WithField("error", err).Panic("register grpc gateway server failed")
		}

		p, _ := runtime.NewPattern(1, []int{2, 0, 2, 1, 4, 1, 5, 2}, []string{"v1", "operations", "name"}, "")
		mux.Handle("GET", p, func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			w.Write([]byte("hello"))
			w.WriteHeader(200)
		})
		// Wire the management REST API and the embedded operations console
		// (served under /console/, proxying /console/api/* to this same HTTP
		// endpoint) in front of the grpc-gateway mux. The scheduler lifecycle
		// is owned by the management API. gRPC-gateway and WebSocket routes
		// are preserved unchanged by the fallback branch. The management
		// handler authenticates internally (legacy fixed token plus session
		// bearer tokens, with /auth/login and /auth/logout open), so it is
		// not wrapped in authGuard.
		consoleToken := ""
		if h.authOn {
			consoleToken = h.authToken
		}
		httpSvc.Handler = &apiRouter{
			gateway:    mux,
			management: managementAPI,
			console: webconsole.NewHandler(webconsole.Config{
				BackendURL: "http://" + httpEndpoint,
				AuthToken:  consoleToken,
			}),
		}
		httpSvc.Addr = httpEndpoint

		// Start http server. ErrServerClosed is expected when the listener is
		// closed by httpSvc.Close() during shutdown, so it is not a failure.
		if err := httpSvc.Serve(listen); err != nil && err != http.ErrServerClosed {
			log.WithField("error", err).Panic("start grpc gateway server failed")
		}
	}()

	log.WithFields(log.Fields{"address": playModule.GetRPCParams().Address, "port": playModule.GetRPCParams().GrpcPort, "token": h.authOn}).Info("grpc server listening")
	log.WithFields(log.Fields{"address": playModule.GetRPCParams().Address, "port": playModule.GetRPCParams().HttpPort, "token": h.authOn}).Info("http server listening")

	<-stopChan
	// Stop the scheduler and the failover monitor before tearing down the
	// listeners so a scheduled task or failover switch can never fire while
	// the servers are winding down.
	if scheduler != nil {
		scheduler.Stop()
	}
	if failoverMonitor != nil {
		failoverMonitor.Stop()
	}
	grpcSvc.Stop()
	_ = httpSvc.Close()
}

func protoErrorHandle(ctx context.Context, mux *runtime.ServeMux, marshaler runtime.Marshaler, writer http.ResponseWriter, request *http.Request, err error) {
	s, ok := status.FromError(err)
	if !ok {
		s = status.New(codes.Unknown, err.Error())
	}

	// set header
	writer.Header().Del("Trailer")
	writer.Header().Set("Context-Type", "application/json")

	// set content
	body := &struct {
		InternalCode codes.Code
		Message      string
		Details      []interface{}
	}{}

	body.InternalCode = s.Code()
	body.Message = err.Error()
	body.Details = s.Details()

	buf, merr := marshaler.Marshal(body)
	if merr != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(merr.Error()))
		return
	}

	// set status
	writer.WriteHeader(runtime.HTTPStatusFromCode(s.Code()))
	_, _ = writer.Write(buf)
}

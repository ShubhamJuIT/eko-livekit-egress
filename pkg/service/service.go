package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/stats"
	"github.com/livekit/egress/version"
	"github.com/livekit/livekit-server/pkg/service/rpc"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
)

const shutdownTimer = time.Second * 30

type Service struct {
	conf        *config.ServiceConfig
	rpcServerV0 egress.RPCServer
	psrpcServer rpc.EgressInternalServer
	ioClient    rpc.IOInfoClient
	promServer  *http.Server
	monitor     *stats.Monitor
	manager     *ProcessManager

	shutdown chan struct{}
}

func NewService(conf *config.ServiceConfig, bus psrpc.MessageBus, rpcServerV0 egress.RPCServer, ioClient rpc.IOInfoClient) (*Service, error) {
	monitor := stats.NewMonitor()

	s := &Service{
		conf:        conf,
		rpcServerV0: rpcServerV0,
		ioClient:    ioClient,
		monitor:     monitor,
		shutdown:    make(chan struct{}),
	}
	s.manager = NewProcessManager(conf, monitor, s.onFatalError)

	psrpcServer, err := rpc.NewEgressInternalServer(conf.NodeID, s, bus)
	if err != nil {
		return nil, err
	}
	s.psrpcServer = psrpcServer

	err = s.psrpcServer.RegisterStartEgressTopic(conf.ClusterId)
	if err != nil {
		return nil, err
	}

	if conf.PrometheusPort > 0 {
		s.promServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", conf.PrometheusPort),
			Handler: promhttp.Handler(),
		}
	}

	return s, nil
}

func (s *Service) Run() error {
	logger.Debugw("starting service", "version", version.Version)

	if s.promServer != nil {
		promListener, err := net.Listen("tcp", s.promServer.Addr)
		if err != nil {
			return err
		}
		go func() {
			_ = s.promServer.Serve(promListener)
		}()
	}

	if err := s.monitor.Start(s.conf, s.isAvailable); err != nil {
		return err
	}

	requests, err := s.rpcServerV0.GetRequestChannel(context.Background())
	if err != nil {
		return err
	}

	defer func() {
		_ = requests.Close()
	}()

	logger.Debugw("service ready")

	for {
		select {
		case <-s.shutdown:
			logger.Infow("shutting down")
			s.psrpcServer.Shutdown()
			for !s.manager.isIdle() {
				time.Sleep(shutdownTimer)
			}
			return nil

		case msg := <-requests.Channel():
			req := &livekit.StartEgressRequest{}
			if err = proto.Unmarshal(requests.Payload(msg), req); err != nil {
				logger.Errorw("malformed request", err)
				continue
			}

			s.handleRequestV0(req)
		}
	}
}

func (s *Service) StartEgress(ctx context.Context, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
	ctx, span := tracer.Start(ctx, "Service.StartEgress")
	defer span.End()

	s.monitor.AcceptRequest(req)
	logger.Infow("request received", "egressID", req.EgressId)

	p, err := config.GetValidatedPipelineConfig(s.conf, req)
	if err != nil {
		return nil, err
	}

	logger.Infow("pipeline config validated successfully", "egressID", req.EgressId, "info", p.Info)

	err = s.manager.launchHandler(req, p.Info, 1)
	if err != nil {
		return nil, err
	}

	return p.Info, nil
}

func (s *Service) StartEgressAffinity(req *livekit.StartEgressRequest) float32 {
	if !s.monitor.CanAcceptRequest(req) {
		// cannot accept
		return 0
	}

	if s.manager.isIdle() {
		// group multiple track and track composite requests.
		// if this instance is idle and another is already handling some, the request will go to that server.
		// this avoids having many instances with one track request each, taking availability from room composite.
		return 0.5
	} else {
		// already handling a request and has available cpu
		return 1
	}
}

func (s *Service) ListActiveEgress(ctx context.Context, _ *rpc.ListActiveEgressRequest) (*rpc.ListActiveEgressResponse, error) {
	ctx, span := tracer.Start(ctx, "Service.ListActiveEgress")
	defer span.End()

	egressIDs := s.manager.listEgress()
	return &rpc.ListActiveEgressResponse{
		EgressIds: egressIDs,
	}, nil
}

func (s *Service) Status() ([]byte, error) {
	return json.Marshal(s.manager.status())
}

func (s *Service) isAvailable() float64 {
	if s.manager.isIdle() {
		return 1
	}
	return 0
}

func (s *Service) onFatalError(info *livekit.EgressInfo) {
	sendUpdate(context.Background(), s.ioClient, info)
	s.Stop(false)
}

func (s *Service) Stop(kill bool) {
	select {
	case <-s.shutdown:
	default:
		close(s.shutdown)
	}

	if kill {
		s.manager.killAll()
	}
}

func (s *Service) KillAll() {
	s.manager.killAll()
}

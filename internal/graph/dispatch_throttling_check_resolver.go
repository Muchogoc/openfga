package graph

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"

	"github.com/openfga/openfga/internal/build"
	"github.com/openfga/openfga/pkg/telemetry"
)

// DispatchThrottlingCheckResolverConfig encapsulates configuration for dispatch throttling check resolver.
type DispatchThrottlingCheckResolverConfig struct {
	Frequency        time.Duration
	DefaultThreshold uint32
	MaxThreshold     uint32
}

// DispatchThrottlingCheckResolver will prioritize requests with fewer dispatches over
// requests with more dispatches.
// Initially, request's dispatches will not be throttled and will be processed
// immediately. When the number of request dispatches is above the DefaultThreshold, the dispatches are placed
// in the throttling queue. One item form the throttling queue will be processed ticker.
// This allows a check / list objects request to be gradually throttled.
type DispatchThrottlingCheckResolver struct {
	delegate        CheckResolver
	config          DispatchThrottlingCheckResolverConfig
	ticker          *time.Ticker
	throttlingQueue chan struct{}
	done            chan struct{}
}

var _ CheckResolver = (*DispatchThrottlingCheckResolver)(nil)

var (
	dispatchThrottlingResolverDelayMsHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:                       build.ProjectName,
		Name:                            "dispatch_throttling_resolver_delay_ms",
		Help:                            "Time spent waiting for dispatch throttling resolver",
		Buckets:                         []float64{1, 3, 5, 10, 25, 50, 100, 1000, 5000}, // Milliseconds. Upper bound is config.UpstreamTimeout.
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	}, []string{"grpc_service", "grpc_method"})
)

func NewDispatchThrottlingCheckResolver(
	config DispatchThrottlingCheckResolverConfig) *DispatchThrottlingCheckResolver {
	dispatchThrottlingCheckResolver := &DispatchThrottlingCheckResolver{
		config:          config,
		ticker:          time.NewTicker(config.Frequency),
		throttlingQueue: make(chan struct{}),
		done:            make(chan struct{}),
	}
	dispatchThrottlingCheckResolver.delegate = dispatchThrottlingCheckResolver
	go dispatchThrottlingCheckResolver.runTicker()
	return dispatchThrottlingCheckResolver
}

func (r *DispatchThrottlingCheckResolver) SetDelegate(delegate CheckResolver) {
	r.delegate = delegate
}

func (r *DispatchThrottlingCheckResolver) GetDelegate() CheckResolver {
	return r.delegate
}

func (r *DispatchThrottlingCheckResolver) Close() {
	r.done <- struct{}{}
}

func (r *DispatchThrottlingCheckResolver) nonBlockingSend(signalChan chan struct{}) {
	select {
	case signalChan <- struct{}{}:
		// message sent
	default:
		// message dropped
	}
}

func (r *DispatchThrottlingCheckResolver) runTicker() {
	for {
		select {
		case <-r.done:
			r.ticker.Stop()
			close(r.done)
			close(r.throttlingQueue)
			return
		case <-r.ticker.C:
			r.nonBlockingSend(r.throttlingQueue)
		}
	}
}

func (r *DispatchThrottlingCheckResolver) ResolveCheck(ctx context.Context,
	req *ResolveCheckRequest,
) (*ResolveCheckResponse, error) {
	ctx, span := tracer.Start(ctx, "ResolveCheck")
	defer span.End()
	span.SetAttributes(attribute.String("resolver_type", "DispatchThrottlingCheckResolver"))

	currentNumDispatch := req.GetRequestMetadata().DispatchCounter.Load()
	span.SetAttributes(attribute.Int("dispatch_count", int(currentNumDispatch)))

	threshold := r.config.DefaultThreshold

	maxThreshold := r.config.MaxThreshold
	if maxThreshold == 0 {
		maxThreshold = r.config.DefaultThreshold
	}

	thresholdInCtx := telemetry.DispatchThrottlingThresholdFromContext(ctx)

	if thresholdInCtx > 0 {
		threshold = min(thresholdInCtx, maxThreshold)
	}

	if currentNumDispatch > threshold {
		req.GetRequestMetadata().WasThrottled.Store(true)

		start := time.Now()
		<-r.throttlingQueue
		end := time.Now()
		timeWaiting := end.Sub(start).Milliseconds()

		rpcInfo := telemetry.RPCInfoFromContext(ctx)
		dispatchThrottlingResolverDelayMsHistogram.WithLabelValues(
			rpcInfo.Service,
			rpcInfo.Method,
		).Observe(float64(timeWaiting))
	}

	return r.delegate.ResolveCheck(ctx, req)
}

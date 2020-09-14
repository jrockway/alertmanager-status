package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

var (
	healthMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "alertmanager_status_alertmanager_health",
			Help: "The current health status of the monitored alertmanager.",
		},
		[]string{"name"},
	)

	lastHealthyMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "alertmanager_status_alertmanager_last_healthy",
			Help: "When the monitored alertmanager last checked in successfully.",
		},
		[]string{"name"},
	)

	lastHealthCheckedMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "alertmanager_status_last_health_checked",
			Help: "When we last received a health check attempt; use this to monitor your third-party monitoring service.",
		},
		[]string{"name"},
	)
)

func init() {
	prometheus.MustRegister(healthMetric, lastHealthyMetric, lastHealthCheckedMetric)
}

type HealthStatus bool

const (
	Unhealthy HealthStatus = false
	Healthy   HealthStatus = true
)

func (h HealthStatus) String() string {
	if h {
		return "healthy"
	}
	return "unhealthy"
}

func (h HealthStatus) AsFloat64() float64 {
	if h {
		return 1.0
	}
	return 0.0
}

const (
	// How long to wait on internal operations.
	internalTimeout = 5 * time.Second
)

// Watcher watches for Alertmanager checkins, logs information when the health status changes, and
// serves an HTTP status page with information about the current state.
//
// The idea is that Watcher acts as an Alertmanager webhook, and you set up an alert to always be
// firing.  When the HTTP handler receives the alert from Alertmanager, the watcher transitions to
// "healthy" for the duration defined by `threshold`.  If the alert stops being fired, eventually we
// become unhealthy and serve that status on the monitoring endpoint.  You can then hook that into
// generic "website down" monitoring and be alerted that you can't receive alerts.
//
// We implement the watcher with a timer that expires a certain time after the last "healthy"
// message.  This is not strictly necessary, but allows us to log a message at the exact instant
// that we start serving an "unhealthy" status.  The code would be much simpler if we just
// subtracted the last healthy time from the current time when someone asked for the status.
type Watcher struct {
	Name string
	C    chan HealthStatus // Writing a health status to C allows you to mark the watcher as healthy or unhealthy.

	cancelCh chan struct{}
	reqCh    chan HealthStatus

	blockCh chan struct{} // Channel that will block the loop, for unit tests.
}

// NewWatcher creates a new watcher in the "unhealthy" state.  To mark the status as healthy, send
// Healthy to watcher.C.
func NewWatcher(l *zap.Logger, name string, threshold time.Duration) *Watcher {
	w := &Watcher{
		Name:     name,
		C:        make(chan HealthStatus),
		cancelCh: make(chan struct{}),
		reqCh:    make(chan HealthStatus),
		blockCh:  make(chan struct{}),
	}
	l = l.Named(name)
	hm := healthMetric.WithLabelValues(name)
	lhm := lastHealthyMetric.WithLabelValues(name)

	go func() {
		var health HealthStatus
		var lastHealthy time.Time
		ticker := time.NewTicker(threshold)
		for {
			l.Debug("trip through the loop", zap.Stringer("health", health), zap.Time("last_healthy", lastHealthy))
			hm.Set(health.AsFloat64())
			lhm.Set(float64(lastHealthy.UnixNano()) / 1e9)

			select {
			case <-w.cancelCh:
				// Watcher cancelled; stop this goroutine.
				l.Debug("watcher canceled")
				close(w.reqCh)
				close(w.C)
				ticker.Stop()
				return

			case newHealth := <-w.C:
				// An explicit status update.
				l.Debug("health status updated", zap.Stringer("health", newHealth))
				if health != newHealth {
					l.Info("health status change", zap.Stringer("health", newHealth), zap.Time("last_healthy", lastHealthy))
					health = newHealth
				}
				if health == Healthy {
					ticker.Reset(threshold)
					lastHealthy = time.Now()
					lhm.SetToCurrentTime()
				}

			case <-ticker.C:
				// A timer tick.  Though not strictly necessary, the timer continues
				// to tick even when we're already unhealthy.  The code is simpler
				// this way.
				l.Debug("tick")
				if health {
					l.Info("health status change", zap.Stringer("health", Unhealthy), zap.Time("last_healthy", lastHealthy))
				}
				health = Unhealthy

			case w.reqCh <- health:
				// A request to read the current health status.
				l.Debug("sent current health status", zap.Stringer("health", health), zap.Time("last_healthy", lastHealthy))
			case <-w.blockCh:
				l.Info("blocking the event loop; you should never see this outside of tests")
				<-w.blockCh
				l.Info("unblocking the event loop")
			}
		}
	}()
	return w
}

// Stop cancels the watcher.  Any future calls into the Watcher will panic.
func (w *Watcher) Stop() {
	close(w.cancelCh)
}

// HandleAlertmanagerPing is an http.HandlerFunc that accepts alerts from Alertmanager via its
// webhook API, and sets the watcher status to Healthy if the alert is well-formed.
//
// A bad request does not change the status to unhealthy; only the timer does that.
func (w *Watcher) HandleAlertmanagerPing(wr http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	l := ctxzap.Extract(ctx).With(zap.String("remote_addr", req.RemoteAddr))
	l.Debug("handling alertmanager ping")
	if req.Method != "POST" {
		l.Error("non-POST request from alertmanager", zap.String("method", req.Method))
		http.Error(wr, "request method must be POST", http.StatusMethodNotAllowed)
		return
	}

	b, err := ioutil.ReadAll(req.Body)
	if err != nil {
		l.Error("problem reading request body", zap.Error(err))
		http.Error(wr, fmt.Sprintf("reading request body: %v", err), http.StatusBadRequest)
		return
	}
	req.Body.Close()

	var data template.Data
	if err := json.Unmarshal(b, &data); err != nil {
		l.Error("problem parsing alertmanager json", zap.Error(err))
		http.Error(wr, fmt.Sprintf("parsing alert: %v", err), http.StatusBadRequest)
		return
	}

	if len(data.Alerts) == 0 {
		l.Info("no alerts received; not changing status to healthy", zap.Any("template", data))
		http.Error(wr, "no alerts", http.StatusBadRequest)
		return
	}

	tctx, c := context.WithTimeout(ctx, internalTimeout)
	defer c()
	select {
	case w.C <- Healthy:
		l.Debug("marked status healthy")
		wr.WriteHeader(http.StatusAccepted)
		return
	case <-tctx.Done():
		l.Error("problem informing watcher of health status", zap.Error(tctx.Err()))
	}
	http.Error(wr, "problem informing watcher of health status", http.StatusInternalServerError)
}

// HandleHealthCheck is an http.HandlerFunc that accepts HTTP requests from an external "website
// monitoring" service, and returns a 200 status code if Alertmanager is healthy, or a 500 Internal
// Server Error code if Alertmanager is unhealthy.  We choose 500 because it is most likely to be
// treated as an error by your health-checking service, even though it is technically incorrect.
//
// This endpoint is NOT intended to be a liveness/readiness/health probe for the alertmanager-status
// service itself!
func (w *Watcher) HandleHealthCheck(wr http.ResponseWriter, req *http.Request) {
	lastHealthCheckedMetric.WithLabelValues(w.Name).SetToCurrentTime()
	ctx := req.Context()
	l := ctxzap.Extract(ctx).With(zap.String("remote_addr", req.RemoteAddr))
	ctx, c := context.WithTimeout(ctx, internalTimeout)
	defer c()
	var health HealthStatus
	select {
	case <-ctx.Done():
		l.Error("problem reading health status", zap.Error(ctx.Err()))
		http.Error(wr, "problem reading health status: "+ctx.Err().Error(), http.StatusRequestTimeout)
		return
	case health = <-w.reqCh:
	}
	wr.Header().Add("content-type", "text/plain; charset=utf-8")
	if health {
		wr.WriteHeader(http.StatusOK)
		wr.Write([]byte(w.Name + " ok")) // nolint:errcheck
		return
	}
	l.Info("serving 'unhealthy' to external health checker")
	wr.WriteHeader(http.StatusInternalServerError)
	wr.Write([]byte(w.Name + " unhealthy")) // nolint:errcheck
}

// HandleLiveness is an http.HandlerFunc that returns status 200 if the event loop is not locked up.
// This works as a liveness probe and a readiness probe.
func (w *Watcher) HandleLiveness(wr http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	l := ctxzap.Extract(ctx).With(zap.String("remote_addr", req.RemoteAddr))
	ctx, c := context.WithTimeout(ctx, internalTimeout)
	defer c()
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case _, ok := <-w.reqCh:
		if !ok {
			err = errors.New("event loop is shutdown")
		}
	}

	if err != nil {
		l.Error("liveness check failed", zap.Error(err))
		http.Error(wr, "liveness check: "+err.Error(), http.StatusInternalServerError)
		return
	}

	wr.Header().Add("content-type", "text/plain; charset=utf-8")
	wr.WriteHeader(http.StatusOK)
	wr.Write([]byte("live")) // nolint:errcheck
}

package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/prometheus/alertmanager/template"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
)

func TestWatcher(t *testing.T) {
	l := zaptest.NewLogger(t)

	w := NewWatcher(l, "TestWatcher", 10*time.Millisecond)
	if got, want := <-w.reqCh, Unhealthy; got != want {
		t.Errorf("initial state:\n  got: %s\n want: %v", got, want)
	}

	w.C <- Healthy
	if got, want := <-w.reqCh, Healthy; got != want {
		t.Errorf("explicitly healthy:\n  got: %s\n want: %v", got, want)
	}

	time.Sleep(20 * time.Millisecond)
	if got, want := <-w.reqCh, Unhealthy; got != want {
		t.Errorf("after waiting the full threshold duration:\n  got: %s\n want: %v", got, want)
	}

	w.C <- Healthy
	w.C <- Unhealthy
	if got, want := <-w.reqCh, Unhealthy; got != want {
		t.Errorf("after explicit set to unhealthy:\n  got: %s\n want: %v", got, want)
	}

	// Wait for things to stop.  It makes zaptest happier.
	w.Stop()
	for ok := true; ok; {
		_, ok = <-w.reqCh
	}
}

type badReader struct{}

func (*badReader) Read(buf []byte) (int, error) {
	return 0, errors.New("bad things")
}

func TestHandleAlertmanagerPing(t *testing.T) {
	testData := []struct {
		name       string
		request    interface{}
		timeout    bool
		wantStatus int
		wantHealth HealthStatus
	}{
		{
			name:       "empty GET",
			request:    nil,
			wantStatus: http.StatusMethodNotAllowed,
			wantHealth: Unhealthy,
		},
		{
			name:       "broken body",
			request:    new(badReader),
			wantStatus: http.StatusBadRequest,
			wantHealth: Unhealthy,
		},
		{
			name:       "invalid json",
			request:    "{",
			wantStatus: http.StatusBadRequest,
			wantHealth: Unhealthy,
		},
		{
			name:       "no alerts",
			request:    &template.Data{Status: "something"},
			wantStatus: http.StatusBadRequest,
			wantHealth: Unhealthy,
		},
		{
			name:       "good alert",
			request:    &template.Data{Alerts: template.Alerts{template.Alert{Status: "ok"}}},
			wantStatus: http.StatusAccepted,
			wantHealth: Healthy,
		},
		{
			name:       "broken watcher",
			request:    &template.Data{Alerts: template.Alerts{template.Alert{Status: "ok"}}},
			timeout:    true,
			wantStatus: http.StatusInternalServerError,
			wantHealth: Unhealthy,
		},
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			l := zaptest.NewLogger(t, zaptest.Level(zapcore.InfoLevel))
			w := NewWatcher(l, "TestHandleAlertmanagerPing."+test.name, time.Second)
			defer func() {
				w.Stop()
				for ok := true; ok; {
					_, ok = <-w.reqCh
				}
			}()

			var req *http.Request
			rec := httptest.NewRecorder()
			switch x := test.request.(type) {
			case *template.Data:
				body, err := json.Marshal(x)
				if err != nil {
					t.Fatalf("marshal json: %v", err)
				}
				req = httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
			case string:
				req = httptest.NewRequest("POST", "/webhook", bytes.NewReader([]byte(x)))
			case *badReader:
				req = httptest.NewRequest("POST", "/webhook", x)
			case nil:
				req = httptest.NewRequest("GET", "/webhook", http.NoBody)

			default:
				t.Fatalf("bad request %T(%v)", test.request, test.request)
			}
			req = req.WithContext(ctxzap.ToContext(context.Background(), l))
			if test.timeout {
				tctx, c := context.WithCancel(req.Context())
				req = req.WithContext(tctx)
				c()
			}
			w.HandleAlertmanagerPing(rec, req)
			if rec.Body.Len() > 0 {
				t.Logf("response: %s", rec.Body.String())
			}
			if got, want := rec.Code, test.wantStatus; got != want {
				t.Errorf("hitting the webhook: status code:\n  got: %v\n want: %v", got, want)
			}
			if got, want := <-w.reqCh, test.wantHealth; got != want {
				t.Errorf("after valid alertmanager ping:\n  got: %s\n want: %v", got, want)
			}
		})
	}
}

func TestHandleHealthCheck(t *testing.T) {
	testData := []struct {
		name       string
		timeout    bool
		health     HealthStatus
		wantStatus int
	}{
		{
			name:       "unhealthy",
			health:     Unhealthy,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "healthy",
			health:     Healthy,
			wantStatus: http.StatusOK,
		},
		{
			name:       "broken",
			health:     Healthy,
			timeout:    true,
			wantStatus: http.StatusRequestTimeout,
		},
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			l := zaptest.NewLogger(t, zaptest.Level(zapcore.InfoLevel))
			w := NewWatcher(l, "TestHandleHealthCheck."+test.name, time.Second)
			defer func() {
				if !test.timeout {
					w.Stop()
				}
				for ok := true; ok; {
					_, ok = <-w.reqCh
				}
			}()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", http.NoBody)
			req = req.WithContext(ctxzap.ToContext(context.Background(), l))
			w.C <- test.health
			if test.timeout {
				tctx, c := context.WithCancel(req.Context())
				req = req.WithContext(tctx)
				c()
				w.Stop()
			}
			w.HandleHealthCheck(rec, req)
			if rec.Body.Len() > 0 {
				t.Logf("response: %s", rec.Body.String())
			}
			if got, want := rec.Code, test.wantStatus; got != want {
				t.Errorf("hitting the webhook: status code:\n  got: %v\n want: %v", got, want)
			}
		})
	}
}

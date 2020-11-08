package selenosis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"time"

	"github.com/alcounit/selenosis/platform"
	"github.com/alcounit/selenosis/selenium"
	"github.com/alcounit/selenosis/tools"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/imdario/mergo"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/websocket"
)

var (
	httpClient = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

//HandleSession ...
func (app *App) HandleSession(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	logger := app.logger.WithFields(logrus.Fields{
		"request_id": uuid.New(),
		"request":    fmt.Sprintf("%s %s", r.Method, r.URL.Path),
	})

	l, err := app.client.List()
	if err != nil {
		logger.Errorf("failed to get active session list: %v", err)
		tools.JSONError(w, "Failed to get browsers list", http.StatusInternalServerError)
		return
	}

	if len(l) >= app.sessionLimit {
		logger.Warnf("active session limit reached: total %d, limit %d", len(l), app.sessionLimit)
		tools.JSONError(w, "session limit reached", http.StatusInternalServerError)
		return
	}

	logger.WithField("time_elapsed", tools.TimeElapsed(start)).Info("session")

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("failed to read request body: %v", err)
		tools.JSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	type request struct {
		DesiredCapabilities selenium.Capabilities `json:"desiredCapabilities"`
		Capabilities        struct {
			AlwaysMatch selenium.Capabilities    `json:"alwaysMatch"`
			FirstMatch  []*selenium.Capabilities `json:"firstMatch"`
		} `json:"capabilities"`
	}

	caps := request{}
	err = json.Unmarshal(body, &caps)
	if err != nil {
		logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("failed to parse request: %v", err)
		tools.JSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	caps.DesiredCapabilities.ValidateCapabilities()
	caps.Capabilities.AlwaysMatch.ValidateCapabilities()

	if caps.DesiredCapabilities.BrowserName != "" && caps.Capabilities.AlwaysMatch.BrowserName != "" {
		caps.DesiredCapabilities = caps.Capabilities.AlwaysMatch
	}

	firstMatchCaps := caps.Capabilities.FirstMatch
	if len(firstMatchCaps) == 0 {
		firstMatchCaps = append(firstMatchCaps, &selenium.Capabilities{})
	}

	var browser *platform.BrowserSpec
	var capabilities selenium.Capabilities

	for _, first := range firstMatchCaps {
		capabilities = caps.DesiredCapabilities
		mergo.Merge(&capabilities, first)
		capabilities.ValidateCapabilities()

		browser, err = app.browsers.Find(capabilities.BrowserName, capabilities.BrowserVersion)
		if err == nil {
			break
		}
	}

	if err != nil {
		logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("requested browser not found: %v", err)
		tools.JSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	image := parseImage(browser.Image)
	template := &platform.ServiceSpec{
		SessionID:             fmt.Sprintf("%s-%s", image, uuid.New()),
		RequestedCapabilities: capabilities,
		Template:              browser,
	}

	logger.WithField("time_elapsed", tools.TimeElapsed(start)).Infof("starting browser from image: %s", template.Template.Image)

	service, err := app.client.Create(template)
	if err != nil {
		logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("failed to start browser: %v", err)
		tools.JSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	cancel := func() {
		service.CancelFunc()
	}

	var resp *http.Response

	service.URL.Path = r.URL.Path

	i := 1
	for ; ; i++ {
		req, _ := http.NewRequest(http.MethodPost, service.URL.String(), bytes.NewReader(body))
		req.Header.Set("X-Forwarded-Selenosis", app.selenosisHost)
		ctx, done := context.WithTimeout(r.Context(), app.browserWaitTimeout)
		rsp, err := httpClient.Do(req.WithContext(ctx))
		defer done()
		select {
		case <-ctx.Done():
			if rsp != nil {
				rsp.Body.Close()
			}
			switch ctx.Err() {
			case context.DeadlineExceeded:
				logger.WithField("time_elapsed", tools.TimeElapsed(start)).Warn("session attempt timeout")
				if i < app.sessionRetryCount {
					continue
				}
				logger.WithField("time_elapsed", tools.TimeElapsed(start)).Warn("service is not ready")
				tools.JSONError(w, "New session attempts retry count exceeded", http.StatusInternalServerError)
			case context.Canceled:
				logger.WithField("time_elapsed", tools.TimeElapsed(start)).Warn("Client disconnected")
			}
			cancel()
			return
		default:
		}
		if err != nil {
			logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("session failed: %v", err)
			tools.JSONError(w, "New session attempts retry count exceeded", http.StatusInternalServerError)
			cancel()
			return
		}
		if rsp.StatusCode == http.StatusNotFound {
			continue
		}
		resp = rsp
		break
	}

	defer resp.Body.Close()

	var msg map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&msg)
	if err != nil {
		cancel()
		logger.WithField("time_elapsed", tools.TimeElapsed(start)).Errorf("unable to read service response: %v", err)
		tools.JSONError(w, "Failed to read service response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	json.NewEncoder(w).Encode(msg)

	logger.WithField("time_elapsed", tools.TimeElapsed(start)).Infof("browser sessionId: %s", service.SessionID)

}

//HandleProxy ...
func (app *App) HandleProxy(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sessionID := vars["sessionId"]
	host := tools.BuildHostPort(sessionID, app.serviceName, app.sidecarPort)

	logger := app.logger.WithFields(logrus.Fields{
		"request_id": uuid.New(),
		"session_id": sessionID,
		"request":    fmt.Sprintf("%s %s", r.Method, r.URL.Path),
	})

	(&httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.Host = host
			r.URL.Host = host
			r.Header.Set("X-Forwarded-Selenosis", app.selenosisHost)
			logger.Info("proxying session")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Errorf("proxying session error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}).ServeHTTP(w, r)

}

//HadleHubStatus ...
func (app *App) HadleHubStatus(w http.ResponseWriter, r *http.Request) {
	logger := app.logger.WithFields(logrus.Fields{
		"request_id": uuid.New(),
		"request":    fmt.Sprintf("%s %s", r.Method, r.URL.Path),
	})

	w.Header().Set("Content-Type", "application/json")

	l, err := app.client.List()
	if err != nil {
		logger.Errorf("hub status: %v", err)
		tools.JSONError(w, "Failed to get browsers list", http.StatusInternalServerError)
	}

	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"value": map[string]interface{}{
				"message": "selenosis up and running",
				"ready":   len(l),
			},
		})

	logger.WithField("active_sessions", len(l)).Infof("hub status")
}

//HandleReverseProxy ...
func (app *App) HandleReverseProxy(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sessionID := vars["sessionId"]
	fragments := strings.Split(r.URL.Path, "/")
	logger := app.logger.WithFields(logrus.Fields{
		"request_id": uuid.New(),
		"session_id": sessionID,
		"request":    fmt.Sprintf("%s %s", r.Method, r.URL.Path),
	})

	(&httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = tools.BuildHostPort(sessionID, app.serviceName, app.sidecarPort)
			r.Header.Set("X-Forwarded-Selenosis", app.selenosisHost)
			logger.Infof("proxying %s", fragments[1])
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Errorf("%s proxying error: %v", fragments[1], err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}).ServeHTTP(w, r)
}

//HandleVNC ...
func (app *App) HandleVNC() websocket.Handler {
	return func(wsconn *websocket.Conn) {
		defer wsconn.Close()

		vars := mux.Vars(wsconn.Request())
		sessionID := vars["sessionId"]

		logger := app.logger.WithFields(logrus.Fields{
			"request_id": uuid.New(),
			"session_id": sessionID,
			"request":    fmt.Sprintf("%s %s", wsconn.Request().Method, wsconn.Request().URL.Path),
		})

		host := tools.BuildHostPort(sessionID, app.serviceName, "5900")
		logger.Infof("vnc request: %s", host)

		var dialer net.Dialer
		conn, err := dialer.DialContext(wsconn.Request().Context(), "tcp", host)
		if err != nil {
			logger.Errorf("vnc connection error: %v", err)
		}
		defer conn.Close()
		wsconn.PayloadType = websocket.BinaryFrame

		go func() {
			io.Copy(wsconn, conn)
			wsconn.Close()
			logger.Errorf("vnc connection closed")
		}()
		io.Copy(conn, wsconn)
		logger.Errorf("vnc client disconnected")
	}
}

//HandleLogs ...
func (app *App) HandleLogs() websocket.Handler {
	return func(wsconn *websocket.Conn) {
		defer wsconn.Close()

		vars := mux.Vars(wsconn.Request())
		sessionID := vars["sessionId"]

		logger := app.logger.WithFields(logrus.Fields{
			"request_id": uuid.New(),
			"session_id": sessionID,
			"request":    fmt.Sprintf("%s %s", wsconn.Request().Method, wsconn.Request().URL.Path),
		})

		logger.Infof("stream logs request: %s", fmt.Sprintf("%s.%s", sessionID, app.serviceName))

		r, err := app.client.Logs(wsconn.Request().Context(), sessionID)
		if err != nil {
			logger.Errorf("stream logs error: %v", err)
		}
		defer r.Close()
		wsconn.PayloadType = websocket.BinaryFrame

		go func() {
			io.Copy(wsconn, r)
			wsconn.Close()
			logger.Errorf("stream logs connection closed")
		}()
		io.Copy(wsconn, r)
		logger.Errorf("stream logs disconnected")
	}
}

//HandleStatus ...
func (app *App) HandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type Status struct {
		Browsers map[string][]string `json:"config"`
		Sessions []*platform.Service `json:"sessions"`
	}

	type Response struct {
		Status    int    `json:"status"`
		Error     error  `json:"err"`
		Selenosis Status `json:"selenosis"`
	}

	sessions, err := app.client.List()
	if err != nil {
		app.logger.Errorf("hub status: %v", err)
		json.NewEncoder(w).Encode(
			Response{
				Status: http.StatusInternalServerError,
				Error:  err,
				Selenosis: Status{
					Browsers: app.browsers.GetBrowserVersions(),
				},
			},
		)
		return
	}

	err = json.NewEncoder(w).Encode(
		Response{
			Status: http.StatusOK,
			Selenosis: Status{
				Browsers: app.browsers.GetBrowserVersions(),
				Sessions: sessions,
			},
		},
	)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("marshal err: %v", err)))
	}
	return
}

func parseImage(image string) (container string) {
	pref, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return "selenoid-browser"
	}
	return pref.ReplaceAllString(image, "-")
}
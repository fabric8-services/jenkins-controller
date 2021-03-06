package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fabric8-services/fabric8-jenkins-idler/internal/cluster"
	pidler "github.com/fabric8-services/fabric8-jenkins-idler/internal/idler"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/model"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/openshift"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/openshift/client"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/tenant"

	"github.com/fabric8-services/fabric8-jenkins-idler/metric"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
)

const (
	// OpenShiftAPIParam is the parameter name under which the OpenShift cluster API URL is passed using
	// Idle, UnIdle and IsIdle.
	OpenShiftAPIParam = "openshift_api_url"
)

var (
	// Recorder to capture events
	Recorder = metric.PrometheusRecorder{}
)

// IdlerAPI defines the REST endpoints of the Idler
type IdlerAPI interface {
	// Idle triggers an idling of the Jenkins service running in the namespace specified in the namespace
	// parameter of the request. A status code of 200 indicates success whereas 500 indicates failure.
	Idle(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// UnIdle triggers an un-idling of the Jenkins service running in the namespace specified in the namespace
	// parameter of the request. A status code of 200 indicates success whereas 500 indicates failure.
	UnIdle(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// IsIdle returns an status struct indicating whether the Jenkins service in the namespace specified in the
	// namespace parameter of the request is currently idle or not.
	// If an error occurs a response with the HTTP status 500 is returned.
	IsIdle(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// Status returns an statusResponse struct indicating the state of the
	// Jenkins service in the namespace specified in the namespace parameter
	// of the request.
	// If an error occurs a response with the HTTP status 400 or 500 is returned.
	Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// ClusterDNSView writes a JSON representation of the current cluster state to the response writer.
	ClusterDNSView(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// Reset deletes a pod and starts a new one
	Reset(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// SetUserIdlerStatus set users status for idler.
	SetUserIdlerStatus(w http.ResponseWriter, r *http.Request, ps httprouter.Params)

	// GetDisabledUserIdlers gets the user status for idler.
	GetDisabledUserIdlers(w http.ResponseWriter, r *http.Request, ps httprouter.Params)
}

type idler struct {
	userIdlers      *openshift.UserIdlerMap
	clusterView     cluster.View
	openShiftClient client.OpenShiftClient
	tenantService   tenant.Service
	disabledUsers   *model.StringSet
}

type status struct {
	IsIdle bool `json:"is_idle"`
}

type userStatus struct {
	Disable []string `json:"disable"`
	Enable  []string `json:"enable"`
}

// NewIdlerAPI creates a new instance of IdlerAPI.
func NewIdlerAPI(
	userIdlers *openshift.UserIdlerMap,
	clusterView cluster.View,
	ts tenant.Service,
	du *model.StringSet) IdlerAPI {
	// Initialize metrics
	Recorder.Initialize()
	return &idler{
		userIdlers:      userIdlers,
		clusterView:     clusterView,
		openShiftClient: client.NewOpenShift(),
		tenantService:   ts,
		disabledUsers:   du,
	}
}

func (api *idler) Idle(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	openShiftAPI, openShiftBearerToken, err := api.getURLAndToken(r)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err)
		return
	}

	for _, service := range pidler.JenkinsServices {
		startTime := time.Now()
		err = api.openShiftClient.Idle(openShiftAPI, openShiftBearerToken, ps.ByName("namespace"), service)
		elapsedTime := time.Since(startTime).Seconds()

		if err != nil {
			Recorder.RecordReqDuration(service, "Idle", http.StatusInternalServerError, elapsedTime)
			respondWithError(w, http.StatusInternalServerError, err)
			return
		}

		Recorder.RecordReqDuration(service, "Idle", http.StatusOK, elapsedTime)
	}

	w.WriteHeader(http.StatusOK)
}

func (api *idler) UnIdle(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {

	openshiftURL, openshiftToken, err := api.getURLAndToken(r)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err)
		return
	}

	ns := strings.TrimSpace(ps.ByName("namespace"))
	if ns == "" {
		err = errors.New("Missing mandatory param namespace")
		respondWithError(w, http.StatusBadRequest, err)
		return
	}

	// may be jenkins is already running and in that case we don't have to do unidle it
	running, err := api.isJenkinsUnIdled(openshiftURL, openshiftToken, ns)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err)
		return
	} else if running {
		log.Infof("Jenkins is already starting/running on %s", ns)
		w.WriteHeader(http.StatusOK)
		return
	}

	// now that jenkins isn't running we need to check if the cluster has reached
	// its maximum capacity
	clusterFull, err := api.tenantService.HasReachedMaxCapacity(openshiftURL, ns)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err)
		return
	} else if clusterFull {
		err := fmt.Errorf("Maximum Resource limit reached on %s for %s", openshiftURL, ns)
		respondWithError(w, http.StatusServiceUnavailable, err)
		return
	}

	// unidle now
	for _, service := range pidler.JenkinsServices {
		startTime := time.Now()

		err = api.openShiftClient.UnIdle(openshiftURL, openshiftToken, ns, service)
		elapsedTime := time.Since(startTime).Seconds()
		if err != nil {
			Recorder.RecordReqDuration(service, "UnIdle", http.StatusInternalServerError, elapsedTime)
			respondWithError(w, http.StatusInternalServerError, err)
			return
		}

		Recorder.RecordReqDuration(service, "UnIdle", http.StatusOK, elapsedTime)
	}

	w.WriteHeader(http.StatusOK)
}

func (api *idler) IsIdle(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	openShiftAPI, openShiftBearerToken, err := api.getURLAndToken(r)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err)
		return
	}

	state, err := api.openShiftClient.State(openShiftAPI, openShiftBearerToken, ps.ByName("namespace"), "jenkins")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	s := status{}
	s.IsIdle = state < model.PodRunning
	writeResponse(w, http.StatusOK, s)
}

func (api *idler) Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	response := &statusResponse{}

	openshiftURL, openshiftToken, err := api.getURLAndToken(r)
	if err != nil {
		response.AppendError(tokenFetchFailed, "failed to obtain openshift token: "+err.Error())
		writeResponse(w, http.StatusBadRequest, *response)
		return
	}

	state, err := api.openShiftClient.State(
		openshiftURL, openshiftToken,
		ps.ByName("namespace"),
		"jenkins",
	)
	if err != nil {
		response.AppendError(openShiftClientError, "openshift client error: "+err.Error())
		writeResponse(w, http.StatusInternalServerError, *response)
		return
	}

	response.SetState(state)
	writeResponse(w, http.StatusOK, *response)
}

func (api *idler) ClusterDNSView(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	writeResponse(w, http.StatusOK, api.clusterView.GetDNSView())
}

func (api *idler) Reset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {

	logger := log.WithFields(log.Fields{"component": "api", "function": "Reset"})

	openShiftAPI, openShiftBearerToken, err := api.getURLAndToken(r)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("{\"error\": \"%s\"}", err)))
		return
	}

	err = api.openShiftClient.Reset(openShiftAPI, openShiftBearerToken, ps.ByName("namespace"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("{\"error\": \"%s\"}", err)))
		return
	}

	w.WriteHeader(http.StatusOK)
}

//SetUserIdlerStatus sets the user status
func (api *idler) SetUserIdlerStatus(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	var users userStatus
	if err := json.NewDecoder(r.Body).Decode(&users); err != nil {
		respondWithError(w, http.StatusBadRequest, err)
		return
	}

	// enabled users will take precedence over disabled
	api.disabledUsers.Add(users.Disable)
	api.disabledUsers.Remove(users.Enable)
	w.WriteHeader(http.StatusOK)
}

type idlerStatusResponse struct {
	Users []string `json:"users,omitempty"`
}

//GetDisabledUserIdlers set the user status
func (api *idler) GetDisabledUserIdlers(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	users := &idlerStatusResponse{Users: api.disabledUsers.Keys()}
	writeResponse(w, http.StatusOK, users)
}

func (api *idler) getURLAndToken(r *http.Request) (string, string, error) {
	var openShiftAPIURL string
	values, ok := r.URL.Query()[OpenShiftAPIParam]
	if !ok || len(values) < 1 {
		return "", "", fmt.Errorf("OpenShift API URL needs to be specified")
	}

	openShiftAPIURL = values[0]
	bearerToken, ok := api.clusterView.GetToken(openShiftAPIURL)
	if ok {
		return openShiftAPIURL, bearerToken, nil
	}
	return "", "", fmt.Errorf("Unknown or invalid OpenShift API URL: %s", openShiftAPIURL)
}

func (api idler) isJenkinsUnIdled(openshiftURL, openshiftToken, namespace string) (bool, error) {
	state, err := api.openShiftClient.State(openshiftURL, openshiftToken, namespace, "jenkins")
	if err != nil {
		return false, err
	}

	status := state == model.PodStarting || state == model.PodRunning
	return status, nil
}

func respondWithError(w http.ResponseWriter, status int, err error) {
	log.Error(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(fmt.Sprintf("{\"error\": \"%s\"}", err)))
}

type responseError struct {
	Code        errorCode `json:"code"`
	Description string    `json:"description"`
}

type jenkinsInfo struct {
	State string `json:"state"`
}

type statusResponse struct {
	Data   *jenkinsInfo    `json:"data,omitempty"`
	Errors []responseError `json:"errors,omitempty"`
}

// ErrorCode is an integer that clients to can use to compare errors
type errorCode uint32

const (
	tokenFetchFailed     errorCode = 1
	openShiftClientError errorCode = 2
)

func (s *statusResponse) AppendError(code errorCode, description string) *statusResponse {
	s.Errors = append(s.Errors, responseError{
		Code:        code,
		Description: description,
	})
	return s
}

func (s *statusResponse) SetState(state model.PodState) *statusResponse {
	s.Data = &jenkinsInfo{State: state.String()}
	return s
}

type any interface{}

func writeResponse(w http.ResponseWriter, status int, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, fmt.Errorf("Could not serialize the response: %s", err))
		return
	}
}

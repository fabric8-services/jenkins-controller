package toggles

import (
	"os"
	"time"

	"fmt"

	"strings"

	"github.com/Unleash/unleash-client-go"
	"github.com/Unleash/unleash-client-go/context"
	"github.com/sirupsen/logrus"
)

const (
	appName         = "jenkins-idler"
	toggleFeature   = "jenkins.idler"
	maxWaitForReady = 10
)

var log = logrus.WithField("component", "unleash")

type unleashToggle struct {
	Features
	unleashClient *unleash.Client
}

// NewUnleashToggle creates a new instance of unleashToggle.
func NewUnleashToggle(hostURL string) (Features, error) {
	unleashClient, err := unleash.NewClient(unleash.WithAppName(appName),
		unleash.WithListener(&listener{}),
		unleash.WithInstanceId(os.Getenv("HOSTNAME")),
		unleash.WithUrl(hostURL),
		unleash.WithMetricsInterval(1*time.Minute),
		unleash.WithRefreshInterval(10*time.Second))

	if err != nil {
		log.Error("Unable to initialize Unleash client.", err)
		return nil, err
	}

	readyChan := unleashClient.Ready()
	select {
	case <-readyChan:
		log.Info("Unleash client initialized and ready.")
	case <-time.After(time.Second * maxWaitForReady):
		return nil, fmt.Errorf("unleash client initalization timed out after %d seconds", maxWaitForReady)
	}

	return &unleashToggle{unleashClient: unleashClient}, nil
}

func (t *unleashToggle) IsIdlerEnabled(uid string) (bool, error) {

	enabled := t.unleashClient.IsEnabled(
		toggleFeature,
		withContext(uid),
		unleash.WithFallback(true)) // NOTE: Enabled for all users unless explictly disabled

	return enabled, nil
}

// withContext creates a context based toggle with the user id as key.
func withContext(uid string) unleash.FeatureOption {
	ctx := context.Context{
		UserId: uid,
	}

	return unleash.WithContext(ctx)
}

type listener struct{}

// OnError prints out errors.
func (l listener) OnError(err error) {
	// TODO See https://github.com/fabric8-services/fabric8-jenkins-idler/issues/106
	if strings.Contains(err.Error(), "invalid character") {
		return
	}
	log.WithField("err", err).Warn("OnError")
}

// OnWarning prints out warning.
func (l listener) OnWarning(warning error) {
	log.WithField("err", warning).Warn("OnWarning")
}

// OnReady prints to the console when the repository is ready.
func (l listener) OnReady() {
	log.Info("Unleash client ready")
}

// OnCount prints to the console when the feature is queried.
func (l listener) OnCount(name string, enabled bool) {
}

// OnSent prints to the console when the server has uploaded metrics.
func (l listener) OnSent(payload unleash.MetricsData) {
	log.WithField("payload", payload).Warn("OnSent")
}

// OnRegistered prints to the console when the client has registered.
func (l listener) OnRegistered(payload unleash.ClientData) {
	log.Info("Unleash client registered")
}

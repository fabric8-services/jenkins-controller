package openshift

import (
	"fmt"
	"strconv"
	"time"

	"context"
	"sync"

	"github.com/fabric8-services/fabric8-jenkins-idler/internal/configuration"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/idler"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/model"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/openshift/client"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/tenant"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/toggles"
	log "github.com/sirupsen/logrus"
)

const (
	availableCond          = "Available"
	channelSendTimeout     = 1
	jenkinsNamespaceSuffix = "-jenkins"
)

var logger = log.WithFields(log.Fields{"component": "controller"})

// Controller defines the interface for watching the OpenShift cluster for changes.
type Controller interface {
	HandleBuild(o model.Object) error
	HandleDeploymentConfig(dc model.DCObject) error
	GetUser(ns string) model.User
}

// controllerImpl watches OpenShift cluser for changes and implements Controller.
type controllerImpl struct {
	users           *UserMap
	userChannels    *UserChannelMap
	openShiftClient client.OpenShiftClient
	tenant          *tenant.Tenant
	features        toggles.Features
	config          configuration.Configuration
	wg              *sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
}

// NewController creates an instance of ControllerImpl.
func NewController(ctx context.Context, openShiftClient client.OpenShiftClient, t *tenant.Tenant, features toggles.Features, config configuration.Configuration, wg *sync.WaitGroup, cancel context.CancelFunc) Controller {
	controller := controllerImpl{
		openShiftClient: openShiftClient,
		users:           NewUserMap(),
		userChannels:    NewUserChannelMap(),
		tenant:          t,
		features:        features,
		config:          config,
		wg:              wg,
		ctx:             ctx,
		cancel:          cancel,
	}

	return &controller
}

// GetUser gets the User for the current namespace.
func (oc *controllerImpl) GetUser(ns string) model.User {
	return oc.userForNamespace(ns)
}

// HandleBuild processes new Build event collected from OpenShift and updates
// user structure with latest build info. NOTE: In most cases the only change in
// build object is stage timestamp, which we don't care about, so this function
// just does couple comparisons and returns.
func (oc *controllerImpl) HandleBuild(o model.Object) error {
	ns := o.Object.Metadata.Namespace
	err := oc.createIfNotExist(ns)
	if err != nil {
		return err
	}

	user := oc.userForNamespace(ns)

	if oc.isActive(&o.Object) {
		lastActive := user.ActiveBuild
		if lastActive.Status.Phase != o.Object.Status.Phase || lastActive.Metadata.Name != o.Object.Metadata.Name {
			user.ActiveBuild = o.Object
			oc.users.Store(ns, user)
			oc.sendUserToIdler(ns, user)
		}
	} else {
		lastDone := user.DoneBuild
		if lastDone.Status.Phase != o.Object.Status.Phase || lastDone.Metadata.Name != o.Object.Metadata.Name {
			user.DoneBuild = o.Object
			oc.users.Store(ns, user)
			oc.sendUserToIdler(ns, user)
		}
	}

	// If we have same build name (space name + build number) in Active and Done build reference, it means last event was transition of an Active build into
	// Done build, we need to clean up the Active build ref.
	if user.ActiveBuild.Metadata.Name == user.DoneBuild.Metadata.Name {
		logger.WithFields(log.Fields{"ns": ns}).Infof("Active and Done builds are the same (%s), cleaning active builds", user.ActiveBuild.Metadata.Name)
		user.ActiveBuild = model.Build{Status: model.Status{Phase: "New"}}
		oc.users.Store(ns, user)
		oc.sendUserToIdler(ns, user)
	}

	return nil
}

// HandleDeploymentConfig processes new DC event collected from OpenShift and updates
// user structure with info about the changes in DC. NOTE: This is important for cases
// like reset tenant and update tenant when DC is updated and Jenkins starts because
// of ConfigChange or manual intervention.
func (oc *controllerImpl) HandleDeploymentConfig(dc model.DCObject) error {
	ns := dc.Object.Metadata.Namespace[:len(dc.Object.Metadata.Namespace)-len(jenkinsNamespaceSuffix)]
	err := oc.createIfNotExist(ns)
	if err != nil {
		return err
	}

	user := oc.userForNamespace(ns)

	c, err := dc.Object.Status.GetByType(availableCond)
	if err != nil {
		return err
	}

	// TODO Verify if we need Generation vs. ObservedGeneration
	// This is either a new version of DC or we existing version waiting to come up.
	if (dc.Object.Metadata.Generation != dc.Object.Status.ObservedGeneration && dc.Object.Spec.Replicas > 0) || dc.Object.Status.UnavailableReplicas > 0 {
		user.JenkinsLastUpdate = time.Now().UTC()
		oc.users.Store(ns, user)
		oc.sendUserToIdler(ns, user)
	}

	// Also check if the event means that Jenkins just started (OS AvailableCondition.Status == true) and update time.
	status, err := strconv.ParseBool(c.Status)
	if err != nil {
		return err
	}

	if status == true {
		user.JenkinsLastUpdate = c.LastUpdateTime
		oc.users.Store(ns, user)
		oc.sendUserToIdler(ns, user)
	}

	return nil
}

// createIfNotExist checks existence of a user in the map, initialise if it does not exist.
func (oc *controllerImpl) createIfNotExist(ns string) error {
	if _, exist := oc.users.Load(ns); exist {
		logger.WithField("ns", ns).Debug("User exists")
		return nil
	}

	logger.WithField("ns", ns).Debug("Creating user")
	ti, err := oc.tenant.GetTenantInfoByNamespace(oc.openShiftClient.GetAPIURL(), ns)
	if err != nil {
		return err
	}

	if ti.Meta.TotalCount > 1 {
		return fmt.Errorf("could not add new user - Tenant service returned multiple items: %d", ti.Meta.TotalCount)
	} else if len(ti.Data) == 0 {
		return fmt.Errorf("could not find tenant in cluster %s for namespace %s: %+v", oc.openShiftClient.GetAPIURL(), ns, ti.Errors)
	}

	newUser := model.NewUser(ti.Data[0].ID, ns)
	oc.users.Store(ns, newUser)
	userIdler := idler.NewUserIdler(newUser, oc.openShiftClient, oc.config, oc.features)
	oc.userChannels.Store(ns, userIdler.GetChannel())
	userIdler.Run(oc.ctx, oc.wg, oc.cancel, time.Duration(oc.config.GetCheckInterval())*time.Minute)

	logger.WithField("ns", ns).Debug("New user recorded")
	return nil
}

func (oc *controllerImpl) userForNamespace(ns string) model.User {
	user, _ := oc.users.Load(ns)
	return user
}

// isActive returns true if build phase suggests a build is active, false otherwise.
func (oc *controllerImpl) isActive(b *model.Build) bool {
	return model.Phases[b.Status.Phase] == 1
}

func (oc *controllerImpl) sendUserToIdler(ns string, user model.User) {
	ch, ok := oc.userChannels.Load(ns)
	if !ok {
		logger.WithField("ns", ns).Error("No channel found for sending user instance")
		return
	}

	select {
	case ch <- user:
	case <-time.After(channelSendTimeout * time.Second):
		logger.WithField("ns", ns).Warn("Unable to send user to channel. Discarding event.")
	}
}

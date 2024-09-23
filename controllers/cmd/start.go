package cmd

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	argoevents "github.com/argoproj/argo-events"
	"github.com/argoproj/argo-events/common/logging"
	"github.com/argoproj/argo-events/controllers"
	"github.com/argoproj/argo-events/controllers/eventbus"
	"github.com/argoproj/argo-events/controllers/eventsource"
	"github.com/argoproj/argo-events/controllers/sensor"
	eventbusv1alpha1 "github.com/argoproj/argo-events/pkg/apis/eventbus/v1alpha1"
	eventsourcev1alpha1 "github.com/argoproj/argo-events/pkg/apis/eventsource/v1alpha1"
	sensorv1alpha1 "github.com/argoproj/argo-events/pkg/apis/sensor/v1alpha1"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	imageEnvVar = "ARGO_EVENTS_IMAGE"
)

type ArgoEventsControllerOpts struct {
	Namespaced       bool
	ManagedNamespace string
	LeaderElection   bool
	MetricsPort      int32
	HealthPort       int32
}

func lookupEnvDurationOr(key string, o time.Duration) *time.Duration {
	logger := logging.NewArgoEventsLogger().Named(eventbus.ControllerName)
	v, found := os.LookupEnv(key)
	if found && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			logger.With(key, v).Fatalw("parsing %s duration", key, zap.Error(err))
		} else {
			return &d
		}
	}
	return &o
}

func Start(eventsOpts ArgoEventsControllerOpts) {
	logger := logging.NewArgoEventsLogger().Named(eventbus.ControllerName)
	config, err := controllers.LoadConfig(func(err error) {
		logger.Errorw("Failed to reload global configuration file", zap.Error(err))
	})
	if err != nil {
		logger.Fatalw("Failed to load global configuration file", zap.Error(err))
	}

	if err = controllers.ValidateConfig(config); err != nil {
		logger.Fatalw("Global configuration file validation failed", zap.Error(err))
	}

	imageName, defined := os.LookupEnv(imageEnvVar)
	if !defined {
		logger.Fatalf("required environment variable '%s' not defined", imageEnvVar)
	}
	opts := ctrl.Options{
		Metrics: metricsserver.Options{
			BindAddress: fmt.Sprintf(":%d", eventsOpts.MetricsPort),
		},
		HealthProbeBindAddress: fmt.Sprintf(":%d", eventsOpts.HealthPort),
	}
	if eventsOpts.Namespaced {
		opts.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				eventsOpts.ManagedNamespace: {},
			},
		}
	}
	if eventsOpts.LeaderElection {
		opts.LeaderElection = true
		opts.LeaderElectionID = "argo-events-controller"
		opts.LeaseDuration = lookupEnvDurationOr("LEADER_ELECTION_LEASE_DURATION", 15*time.Second)
		opts.RenewDeadline = lookupEnvDurationOr("LEADER_ELECTION_RENEW_DEADLINE", 10*time.Second)
		opts.RetryPeriod = lookupEnvDurationOr("LEADER_ELECTION_RETRY_PERIOD", 5*time.Second)
	}
	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, opts)
	if err != nil {
		logger.Fatalw("Unable to get a controller-runtime manager", zap.Error(err))
	}
	kubeClient := kubernetes.NewForConfigOrDie(restConfig)

	// Readyness probe
	if err := mgr.AddReadyzCheck("readiness", healthz.Ping); err != nil {
		logger.Fatalw("Unable add a readiness check", zap.Error(err))
	}

	// Liveness probe
	if err := mgr.AddHealthzCheck("liveness", healthz.Ping); err != nil {
		logger.Fatalw("Unable add a health check", zap.Error(err))
	}

	if err := eventbusv1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		logger.Fatalw("Unable to add scheme", zap.Error(err))
	}

	if err := eventsourcev1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		logger.Fatalw("Unable to add EventSource scheme", zap.Error(err))
	}

	if err := sensorv1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		logger.Fatalw("Unable to add Sensor scheme", zap.Error(err))
	}

	// EventBus controller
	eventBusController, err := controller.New(eventbus.ControllerName, mgr, controller.Options{
		Reconciler: eventbus.NewReconciler(mgr.GetClient(), kubeClient, mgr.GetScheme(), config, logger),
	})
	if err != nil {
		logger.Fatalw("Unable to set up EventBus controller", zap.Error(err))
	}

	// Watch EventBus and enqueue EventBus object key
	if err := eventBusController.Watch(source.Kind(mgr.GetCache(), &eventbusv1alpha1.EventBus{}), &handler.EnqueueRequestForObject{},
		predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		)); err != nil {
		logger.Fatalw("Unable to watch EventBus", zap.Error(err))
	}

	// Watch ConfigMaps and enqueue owning EventBus key
	if err := eventBusController.Watch(source.Kind(mgr.GetCache(), &corev1.ConfigMap{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &eventbusv1alpha1.EventBus{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch ConfigMaps", zap.Error(err))
	}

	// Watch StatefulSets and enqueue owning EventBus key
	if err := eventBusController.Watch(source.Kind(mgr.GetCache(), &appv1.StatefulSet{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &eventbusv1alpha1.EventBus{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch StatefulSets", zap.Error(err))
	}

	// Watch Services and enqueue owning EventBus key
	if err := eventBusController.Watch(source.Kind(mgr.GetCache(), &corev1.Service{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &eventbusv1alpha1.EventBus{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch Services", zap.Error(err))
	}

	// EventSource controller
	eventSourceController, err := controller.New(eventsource.ControllerName, mgr, controller.Options{
		Reconciler: eventsource.NewReconciler(mgr.GetClient(), mgr.GetScheme(), imageName, logger),
	})
	if err != nil {
		logger.Fatalw("Unable to set up EventSource controller", zap.Error(err))
	}

	// Watch EventSource and enqueue EventSource object key
	if err := eventSourceController.Watch(source.Kind(mgr.GetCache(), &eventsourcev1alpha1.EventSource{}), &handler.EnqueueRequestForObject{},
		predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		)); err != nil {
		logger.Fatalw("Unable to watch EventSources", zap.Error(err))
	}

	// Watch Deployments and enqueue owning EventSource key
	if err := eventSourceController.Watch(source.Kind(mgr.GetCache(), &appv1.Deployment{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &eventsourcev1alpha1.EventSource{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch Deployments", zap.Error(err))
	}

	// Watch Services and enqueue owning EventSource key
	if err := eventSourceController.Watch(source.Kind(mgr.GetCache(), &corev1.Service{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &eventsourcev1alpha1.EventSource{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch Services", zap.Error(err))
	}

	// Sensor controller
	sensorController, err := controller.New(sensor.ControllerName, mgr, controller.Options{
		Reconciler: sensor.NewReconciler(mgr.GetClient(), mgr.GetScheme(), imageName, logger),
	})
	if err != nil {
		logger.Fatalw("Unable to set up Sensor controller", zap.Error(err))
	}

	// Watch Sensor and enqueue Sensor object key
	if err := sensorController.Watch(source.Kind(mgr.GetCache(), &sensorv1alpha1.Sensor{}), &handler.EnqueueRequestForObject{},
		predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		)); err != nil {
		logger.Fatalw("Unable to watch Sensors", zap.Error(err))
	}

	// Watch Deployments and enqueue owning Sensor key
	if err := sensorController.Watch(source.Kind(mgr.GetCache(), &appv1.Deployment{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &sensorv1alpha1.Sensor{}, handler.OnlyControllerOwner()),
		predicate.GenerationChangedPredicate{}); err != nil {
		logger.Fatalw("Unable to watch Deployments", zap.Error(err))
	}

	logger.Infow("Starting controller manager", "version", argoevents.GetVersion())
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		logger.Fatalw("Unable to start controller manager", zap.Error(err))
	}
}

package scheduler

import (
	"sync"

	"code.cloudfoundry.org/lager"
	"github.com/carlescere/scheduler"

	"github.com/alphagov/paas-rds-metric-collector/pkg/brokerinfo"
	"github.com/alphagov/paas-rds-metric-collector/pkg/collector"
	"github.com/alphagov/paas-rds-metric-collector/pkg/config"
	"github.com/alphagov/paas-rds-metric-collector/pkg/emitter"
	"github.com/alphagov/paas-rds-metric-collector/pkg/metrics"
	"github.com/alphagov/paas-rds-metric-collector/pkg/utils"
)

type collectorWorker struct {
	collector collector.MetricsCollector
	job       *scheduler.Job
}

// Scheduler ...
type Scheduler struct {
	brokerinfo             brokerinfo.BrokerInfo
	metricsEmitter         emitter.MetricsEmitter
	metricsCollectorDriver collector.MetricsCollectorDriver

	instanceRefreshInterval int
	metricCollectorInterval int

	logger lager.Logger

	workers map[string]*collectorWorker
	job     *scheduler.Job
	mux     sync.Mutex
}

// NewScheduler ...
func NewScheduler(
	schedulerConfig config.SchedulerConfig,
	brokerInfo brokerinfo.BrokerInfo,
	metricsEmitter emitter.MetricsEmitter,
	metricsCollectorDriver collector.MetricsCollectorDriver,
	logger lager.Logger,
) *Scheduler {

	return &Scheduler{
		brokerinfo:             brokerInfo,
		metricsEmitter:         metricsEmitter,
		metricsCollectorDriver: metricsCollectorDriver,

		instanceRefreshInterval: schedulerConfig.InstanceRefreshInterval,
		metricCollectorInterval: schedulerConfig.MetricCollectorInterval,

		workers: map[string]*collectorWorker{},

		logger: logger,
	}
}

// Start ...
func (w *Scheduler) Start() error {
	var err error
	w.job, err = scheduler.Every(w.instanceRefreshInterval).Seconds().Run(func() {
		w.mux.Lock()
		defer w.mux.Unlock()

		serviceInstances, err := w.brokerinfo.ListInstanceGUIDs()
		if err != nil {
			w.logger.Error("unable to retreive instance guids", err)
			return
		}

		for _, instanceGUID := range serviceInstances {
			if w.WorkerExists(instanceGUID) {
				continue
			}
			w.StartWorker(instanceGUID)
		}

		for _, instanceGUID := range w.ListWorkers() {
			if !utils.SliceContainsString(serviceInstances, instanceGUID) {
				w.StopWorker(instanceGUID)
			}
		}
	})
	return err
}

// Stop
func (w *Scheduler) Stop() {
	w.mux.Lock()
	defer w.mux.Unlock()
	w.job.Quit <- true
	for _, id := range w.ListWorkers() {
		w.StopWorker(id)
	}
}

// StartWorker ...
func (w *Scheduler) StartWorker(instanceGUID string) {
	w.logger.Info("start_worker", lager.Data{
		"guid": instanceGUID,
	})

	collector, err := w.metricsCollectorDriver.NewCollector(instanceGUID)
	if err != nil {
		w.logger.Error("starting worker collector", err, lager.Data{
			"guid": instanceGUID,
		})
		return
	}

	newJob, err := scheduler.Every(w.metricCollectorInterval).Seconds().Run(func() {
		w.logger.Debug("collecting metrics", lager.Data{
			"guid": instanceGUID,
		})
		collectedMetrics, err := collector.Collect()
		if err != nil {
			w.logger.Error("querying metrics", err, lager.Data{
				"instanceGUID": instanceGUID,
			})
			return
		}
		w.logger.Debug("collected metrics", lager.Data{
			"guid":    instanceGUID,
			"metrics": collectedMetrics,
		})
		for _, metric := range collectedMetrics {
			w.metricsEmitter.Emit(
				metrics.MetricEnvelope{InstanceGUID: instanceGUID, Metric: metric},
			)
		}
	})
	if err != nil {
		w.logger.Error("cannot schedule the worker", err, lager.Data{
			"instanceGUID": instanceGUID,
		})
		return
	}
	w.workers[instanceGUID] = &collectorWorker{
		collector: collector,
		job:       newJob,
	}
}

// StopCollector ...
func (w *Scheduler) StopWorker(instanceGUID string) {
	w.logger.Info("stop_worker", lager.Data{
		"guid": instanceGUID,
	})

	if w.WorkerExists(instanceGUID) {
		err := w.workers[instanceGUID].collector.Close()
		if err != nil {
			w.logger.Error("close_collector", err, lager.Data{
				"instanceGUID": instanceGUID,
			})
		}
		w.workers[instanceGUID].job.Quit <- true
	}
	delete(w.workers, instanceGUID)
}

// WorkerExists ...
func (w *Scheduler) WorkerExists(instanceGUID string) bool {
	_, ok := w.workers[instanceGUID]
	return ok
}

// ListWorkers ...
func (w *Scheduler) ListWorkers() []string {
	keys := make([]string, 0, len(w.workers))
	for k := range w.workers {
		keys = append(keys, k)
	}

	return keys
}
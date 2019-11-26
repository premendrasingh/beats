// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package operation

import (
	"fmt"
	"os"
	"sync"

	"github.com/pkg/errors"

	"github.com/elastic/beats/x-pack/agent/pkg/agent/configrequest"
	operatorCfg "github.com/elastic/beats/x-pack/agent/pkg/agent/operation/config"
	"github.com/elastic/beats/x-pack/agent/pkg/agent/stateresolver"
	"github.com/elastic/beats/x-pack/agent/pkg/artifact/download"
	"github.com/elastic/beats/x-pack/agent/pkg/artifact/install"
	"github.com/elastic/beats/x-pack/agent/pkg/config"
	"github.com/elastic/beats/x-pack/agent/pkg/core/logger"
	"github.com/elastic/beats/x-pack/agent/pkg/core/plugin/app"
	"github.com/elastic/beats/x-pack/agent/pkg/core/plugin/state"
	rconfig "github.com/elastic/beats/x-pack/agent/pkg/core/remoteconfig/grpc"
)

// Operator runs Start/Stop/Update operations
// it is responsible for detecting reconnect to existing processes
// based on backed up configuration
// Enables running sidecars for processes.
// TODO: implement retry strategies
type Operator struct {
	logger         *logger.Logger
	config         *operatorCfg.Config
	handlers       map[string]handleFunc
	stateResolver  *stateresolver.StateResolver
	eventProcessor callbackHooks

	apps     map[string]Application
	appsLock sync.Mutex

	downloader download.Downloader
	installer  install.Installer
}

// NewOperator creates a new operator, this operator holds
// a collection of running processes, back it up
// Based on backed up collection it prepares clients, watchers... on init
func NewOperator(
	logger *logger.Logger,
	config *config.Config,
	fetcher download.Downloader,
	installer install.Installer,
	stateResolver *stateresolver.StateResolver,
	eventProcessor callbackHooks) (*Operator, error) {

	operatorConfig := &operatorCfg.Config{}
	if err := config.Unpack(&operatorConfig); err != nil {
		return nil, err
	}

	if operatorConfig.DownloadConfig == nil {
		return nil, fmt.Errorf("artifacts configuration not provided")
	}

	if eventProcessor == nil {
		eventProcessor = &noopCallbackHooks{}
	}

	operator := &Operator{
		config:         operatorConfig,
		logger:         logger,
		downloader:     fetcher,
		installer:      installer,
		stateResolver:  stateResolver,
		apps:           make(map[string]Application),
		eventProcessor: eventProcessor,
	}

	operator.initHandlerMap()

	os.MkdirAll(operatorConfig.DownloadConfig.TargetDirectory, 0755)
	os.MkdirAll(operatorConfig.DownloadConfig.InstallPath, 0755)

	return operator, nil
}

// State describes the current state of the system.
// Reports all known beats and theirs states. Whether they are running
// or not, and if they are information about process is also present.
func (o *Operator) State() map[string]state.State {
	result := make(map[string]state.State)

	o.appsLock.Lock()
	defer o.appsLock.Unlock()

	for k, v := range o.apps {
		result[k] = v.State()
	}

	return result
}

// HandleConfig handles configuration for a pipeline and performs actions to achieve this configuration.
func (o *Operator) HandleConfig(cfg configrequest.Request) error {
	_, steps, err := o.stateResolver.Resolve(cfg)
	if err != nil {
		return errors.Wrapf(err, "operator: failed to resolve configuration %s, error: %v", cfg, err)
	}

	for _, step := range steps {
		handler, found := o.handlers[step.ID]
		if !found {
			return fmt.Errorf("operator: received unexpected event '%s'", step.ID)
		}

		if err := handler(step); err != nil {
			return errors.Wrapf(err, "operator: failed to execute step %s, error: %v", step.ID, err)
		}
	}

	return nil
}

// Start starts a new process based on a configuration
// specific configuration of new process is passed
func (o *Operator) start(p Descriptor, cfg map[string]interface{}) (err error) {
	flow := []operation{
		newOperationFetch(o.logger, p, o.config, o.downloader, o.eventProcessor),
		newOperationVerify(o.eventProcessor),
		newOperationInstall(o.logger, p, o.config, o.installer, o.eventProcessor),
		newOperationStart(o.logger, o.config, cfg, o.eventProcessor),
		newOperationConfig(o.logger, o.config, cfg, o.eventProcessor),
	}
	return o.runFlow(p, flow)
}

// Stop stops the running process, if process is already stopped it does not return an error
func (o *Operator) stop(p Descriptor) (err error) {
	flow := []operation{
		newOperationStop(o.logger, o.config, o.eventProcessor),
	}

	return o.runFlow(p, flow)
}

// PushConfig tries to push config to a running process
func (o *Operator) pushConfig(p Descriptor, cfg map[string]interface{}) error {
	var flow []operation
	configurable, err := p.IsGrpcConfigurable(o.config.DownloadConfig)
	if err != nil {
		return err
	}

	if configurable {
		flow = []operation{
			newOperationConfig(o.logger, o.config, cfg, o.eventProcessor),
		}
	} else {
		flow = []operation{
			// updates a configuration file and restarts a process
			newOperationStop(o.logger, o.config, o.eventProcessor),
			newOperationStart(o.logger, o.config, cfg, o.eventProcessor),
		}
	}

	return o.runFlow(p, flow)
}

func (o *Operator) runFlow(p Descriptor, operations []operation) error {
	if len(operations) == 0 {
		o.logger.Infof("operator received event with no operations for program '%s'", p.ID())
		return nil
	}

	app, err := o.getApp(p)
	if err != nil {
		return err
	}

	for _, op := range operations {
		shouldRun, err := op.Check()
		if err != nil {
			return err
		}

		if !shouldRun {
			o.logger.Infof("operation '%s' skipped for %s.%s", op.Name(), p.BinaryName(), p.Version())
			continue
		}

		if err := op.Run(app); err != nil {
			return err
		}
	}

	return nil
}

func (o *Operator) getApp(p Descriptor) (Application, error) {
	o.appsLock.Lock()
	defer o.appsLock.Unlock()

	id := p.ID()

	if a, ok := o.apps[id]; ok {
		return a, nil
	}

	factory := rconfig.NewConnFactory(o.config.RetryConfig.Delay, o.config.RetryConfig.MaxDelay)

	specifier, ok := p.(app.Specifier)
	if !ok {
		return nil, fmt.Errorf("descriptor is not an app.Specifier")
	}

	a := app.NewApplication(p.ID(), p.BinaryName(), specifier, factory, o.config, o.logger, o.eventProcessor.OnFailing)
	o.apps[id] = a
	return a, nil
}
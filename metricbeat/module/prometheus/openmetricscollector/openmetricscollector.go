// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package openmetricscollector

import (
	"regexp"

	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/textparse"

	"github.com/elastic/beats/v7/libbeat/common"
	p "github.com/elastic/beats/v7/metricbeat/helper/openmetrics"
	"github.com/elastic/beats/v7/metricbeat/mb"
	"github.com/elastic/beats/v7/metricbeat/mb/parse"
)

const (
	defaultScheme = "http"
	defaultPath   = "/metrics"
)

var (
	// HostParser parses a OpenMetrics endpoint URL
	HostParser = parse.URLHostParserBuilder{
		DefaultScheme: defaultScheme,
		DefaultPath:   defaultPath,
		PathConfigKey: "metrics_path",
	}.Build()

	upMetricName          = "up"
	upMetricType          = textparse.MetricTypeGauge
	upMetricInstanceLabel = "instance"
	upMetricJobLabel      = "job"
	upMetricJobValue      = "prometheus"
)

func init() {
	mb.Registry.MustAddMetricSet("prometheus", "openmetricscollector",
		MetricSetBuilder("openmetrics", DefaultOpenMetricsEventsGeneratorFactory),
		mb.WithHostParser(HostParser),
		mb.DefaultMetricSet(),
	)
}

// OpenMetricsEventsGenerator converts a OpenMetrics metric family into a OpenMetricEvent list
type OpenMetricsEventsGenerator interface {
	// Start must be called before using the generator
	Start()

	// converts a OpenMetrics metric family into a list of PromEvents
	GenerateOpenMetricsEvents(mf *p.OpenMetricFamily) []OpenMetricEvent

	// Stop must be called when the generator won't be used anymore
	Stop()
}

// OpenMetricsEventsGeneratorFactory creates a OpenMetricsEventsGenerator when instanciating a metricset
type OpenMetricsEventsGeneratorFactory func(ms mb.BaseMetricSet) (OpenMetricsEventsGenerator, error)

// MetricSet for fetching openmetrics data
type MetricSet struct {
	mb.BaseMetricSet
	openmetrics     p.OpenMetrics
	includeMetrics  []*regexp.Regexp
	excludeMetrics  []*regexp.Regexp
	namespace       string
	promEventsGen   OpenMetricsEventsGenerator
	host            string
	eventGenStarted bool
}

// MetricSetBuilder returns a builder function for a new OpenMetrics metricset using
// the given namespace and event generator
func MetricSetBuilder(namespace string, genFactory OpenMetricsEventsGeneratorFactory) func(base mb.BaseMetricSet) (mb.MetricSet, error) {
	return func(base mb.BaseMetricSet) (mb.MetricSet, error) {
		config := defaultConfig
		if err := base.Module().UnpackConfig(&config); err != nil {
			return nil, err
		}
		openmetrics, err := p.NewOpenMetricsClient(base)
		if err != nil {
			return nil, err
		}

		promEventsGen, err := genFactory(base)
		if err != nil {
			return nil, err
		}

		ms := &MetricSet{
			BaseMetricSet:   base,
			openmetrics:     openmetrics,
			namespace:       namespace,
			promEventsGen:   promEventsGen,
			eventGenStarted: false,
		}
		// store host here to use it as a pointer when building `up` metric
		ms.host = ms.Host()
		ms.excludeMetrics, err = p.CompilePatternList(config.MetricsFilters.ExcludeMetrics)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to compile exclude patterns")
		}
		ms.includeMetrics, err = p.CompilePatternList(config.MetricsFilters.IncludeMetrics)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to compile include patterns")
		}

		return ms, nil
	}
}

// Fetch fetches data and reports it
func (m *MetricSet) Fetch(reporter mb.ReporterV2) error {
	if !m.eventGenStarted {
		m.promEventsGen.Start()
		m.eventGenStarted = true
	}

	families, err := m.openmetrics.GetFamilies()
	eventList := map[textparse.MetricType]map[string]common.MapStr{}
	if err != nil {
		// send up event only
		families = append(families, m.upMetricFamily(0.0))

		// set the error to report it after sending the up event
		err = errors.Wrap(err, "unable to decode response from openmetrics endpoint")
	} else {
		// add up event to the list
		families = append(families, m.upMetricFamily(1.0))
	}

	for _, family := range families {
		if m.skipFamily(family) {
			continue
		}
		promEvents := m.promEventsGen.GenerateOpenMetricsEvents(family)

		for _, promEvent := range promEvents {
			labelsHash := promEvent.LabelsHash()
			if _, ok := eventList[promEvent.Type]; !ok {
				eventList[promEvent.Type] = make(map[string]common.MapStr)
			}
			if _, ok := eventList[promEvent.Type][labelsHash]; !ok {
				eventList[promEvent.Type][labelsHash] = common.MapStr{}

				// Add default instance label if not already there
				if exists, _ := promEvent.Labels.HasKey(upMetricInstanceLabel); !exists {
					promEvent.Labels.Put(upMetricInstanceLabel, m.Host())
				}
				// Add default job label if not already there
				if exists, _ := promEvent.Labels.HasKey("job"); !exists {
					promEvent.Labels.Put("job", m.Module().Name())
				}
				// Add labels
				if len(promEvent.Labels) > 0 {
					eventList[promEvent.Type][labelsHash]["labels"] = promEvent.Labels
				}
			}

			//if promEvent.Help != "" {
			//	eventList[labelsHash]["help"] = promEvent.Help
			//}
			if promEvent.Type != "" {
				eventList[promEvent.Type][labelsHash]["type"] = promEvent.Type
			}
			if promEvent.Unit != "" {
				eventList[promEvent.Type][labelsHash]["unit"] = promEvent.Unit
			}
			if promEvent.Created > 0 {
				eventList[promEvent.Type][labelsHash]["created"] = promEvent.Created
			}
			//if promEvent.TimestampMs > 0 {
			//	eventList[labelsHash]["timestamp_ms"] = promEvent.TimestampMs
			//}

			if len(promEvent.Exemplars) > 0 {
				eventList[promEvent.Type][labelsHash]["exemplar"] = promEvent.Exemplars
			}
			// Accumulate metrics in the event
			eventList[promEvent.Type][labelsHash].DeepUpdate(promEvent.Data)
		}
	}

	// Report events
	for _, e := range eventList {
		for _, ev := range e {
			isOpen := reporter.Event(mb.Event{
				RootFields: common.MapStr{m.namespace: ev},
			})
			if !isOpen {
				break
			}
		}
	}

	return err
}

// Close stops the metricset
func (m *MetricSet) Close() error {
	if m.eventGenStarted {
		m.promEventsGen.Stop()
	}
	return nil
}

func (m *MetricSet) upMetricFamily(value float64) *p.OpenMetricFamily {
	gauge := p.Gauge{
		Value: &value,
	}
	label1 := labels.Label{
		Name:  upMetricInstanceLabel,
		Value: m.host,
	}
	label2 := labels.Label{
		Name:  upMetricJobLabel,
		Value: upMetricJobValue,
	}
	metric := p.OpenMetric{
		Gauge: &gauge,
		Label: []*labels.Label{&label1, &label2},
	}
	return &p.OpenMetricFamily{
		Name:   &upMetricName,
		Type:   textparse.MetricType(upMetricType),
		Metric: []*p.OpenMetric{&metric},
	}
}

func (m *MetricSet) skipFamily(family *p.OpenMetricFamily) bool {
	if family == nil || family.Name == nil {
		return false
	}
	return m.skipFamilyName(*family.Name)
}

func (m *MetricSet) skipFamilyName(family string) bool {
	// example:
	//	include_metrics:
	//		- node_*
	//	exclude_metrics:
	//		- node_disk_*
	//
	// This would mean that we want to keep only the metrics that start with node_ prefix but
	// are not related to disk so we exclude node_disk_* metrics from them.

	// if include_metrics are defined, check if this metric should be included
	if len(m.includeMetrics) > 0 {
		if !p.MatchMetricFamily(family, m.includeMetrics) {
			return true
		}
	}
	// now exclude the metric if it matches any of the given patterns
	if len(m.excludeMetrics) > 0 {
		if p.MatchMetricFamily(family, m.excludeMetrics) {
			return true
		}
	}
	return false
}

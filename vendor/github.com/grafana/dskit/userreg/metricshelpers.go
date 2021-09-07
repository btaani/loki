package userreg

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var (
	bytesBufferPool = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(nil)
		},
	}
)

// MetricFamilyMap is a map of metric names to their family (metrics with same name, but different labels)
// Keeping map of metric name to its family makes it easier to do searches later.
type MetricFamilyMap map[string]*dto.MetricFamily

// NewMetricFamilyMap sorts output from Gatherer.Gather method into a map.
// Gatherer.Gather specifies that there metric families are uniquely named, and we use that fact here.
// If they are not, this method returns error.
func NewMetricFamilyMap(metrics []*dto.MetricFamily) (MetricFamilyMap, error) {
	perMetricName := MetricFamilyMap{}

	for _, m := range metrics {
		name := m.GetName()
		// these errors should never happen when passing Gatherer.Gather() output.
		if name == "" {
			return nil, errors.New("empty name for metric family")
		}
		if perMetricName[name] != nil {
			return nil, fmt.Errorf("non-unique name for metric family: %q", name)
		}

		perMetricName[name] = m
	}

	return perMetricName, nil
}

func (mfm MetricFamilyMap) SumCounters(name string) float64 {
	return sum(mfm[name], counterValue)
}

func (mfm MetricFamilyMap) SumGauges(name string) float64 {
	return sum(mfm[name], gaugeValue)
}

func (mfm MetricFamilyMap) MaxGauges(name string) float64 {
	return max(mfm[name], gaugeValue)
}

func (mfm MetricFamilyMap) SumHistograms(name string) HistogramData {
	hd := HistogramData{}
	mfm.SumHistogramsTo(name, &hd)
	return hd
}

func (mfm MetricFamilyMap) SumHistogramsTo(name string, output *HistogramData) {
	for _, m := range mfm[name].GetMetric() {
		output.AddHistogram(m.GetHistogram())
	}
}

func (mfm MetricFamilyMap) SumSummaries(name string) SummaryData {
	sd := SummaryData{}
	mfm.SumSummariesTo(name, &sd)
	return sd
}

func (mfm MetricFamilyMap) SumSummariesTo(name string, output *SummaryData) {
	for _, m := range mfm[name].GetMetric() {
		output.AddSummary(m.GetSummary())
	}
}

func (mfm MetricFamilyMap) sumOfSingleValuesWithLabels(metric string, labelNames []string, extractFn func(*dto.Metric) float64, aggregateFn func(labelsKey string, labelValues []string, value float64)) {
	metricsPerLabelValue := getMetricsWithLabelNames(mfm[metric], labelNames)

	for key, mlv := range metricsPerLabelValue {
		for _, m := range mlv.metrics {
			val := extractFn(m)
			aggregateFn(key, mlv.labelValues, val)
		}
	}
}

// sum returns sum of values from all metrics from same metric family (= series with the same metric name, but different labels)
// Supplied function extracts value.
func sum(mf *dto.MetricFamily, fn func(*dto.Metric) float64) float64 {
	result := float64(0)
	for _, m := range mf.GetMetric() {
		result += fn(m)
	}
	return result
}

// max returns the max value from all metrics from same metric family (= series with the same metric name, but different labels)
// Supplied function extracts value.
func max(mf *dto.MetricFamily, fn func(*dto.Metric) float64) float64 {
	result := math.NaN()

	for _, m := range mf.GetMetric() {
		if value := fn(m); math.IsNaN(result) || value > result {
			result = value
		}
	}

	// If there's no metric, we do return 0 which is the gauge default.
	if math.IsNaN(result) {
		return 0
	}

	return result
}

// This works even if m is nil, m.Counter is nil or m.Counter.Value is nil (it returns 0 in those cases)
func counterValue(m *dto.Metric) float64 { return m.GetCounter().GetValue() }
func gaugeValue(m *dto.Metric) float64   { return m.GetGauge().GetValue() }

// SummaryData keeps all data needed to create summary metric
type SummaryData struct {
	sampleCount uint64
	sampleSum   float64
	quantiles   map[float64]float64
}

func (s *SummaryData) AddSummary(sum *dto.Summary) {
	s.sampleCount += sum.GetSampleCount()
	s.sampleSum += sum.GetSampleSum()

	qs := sum.GetQuantile()
	if len(qs) > 0 && s.quantiles == nil {
		s.quantiles = map[float64]float64{}
	}

	for _, q := range qs {
		// we assume that all summaries have same quantiles
		s.quantiles[q.GetQuantile()] += q.GetValue()
	}
}

func (s *SummaryData) Metric(desc *prometheus.Desc, labelValues ...string) prometheus.Metric {
	return prometheus.MustNewConstSummary(desc, s.sampleCount, s.sampleSum, s.quantiles, labelValues...)
}

// HistogramData keeps data required to build histogram Metric
type HistogramData struct {
	sampleCount uint64
	sampleSum   float64
	buckets     map[float64]uint64
}

// Adds histogram from gathered metrics to this histogram data.
func (d *HistogramData) AddHistogram(histo *dto.Histogram) {
	d.sampleCount += histo.GetSampleCount()
	d.sampleSum += histo.GetSampleSum()

	histoBuckets := histo.GetBucket()
	if len(histoBuckets) > 0 && d.buckets == nil {
		d.buckets = map[float64]uint64{}
	}

	for _, b := range histoBuckets {
		// we assume that all histograms have same buckets
		d.buckets[b.GetUpperBound()] += b.GetCumulativeCount()
	}
}

// Merged another histogram data into this one.
func (d *HistogramData) AddHistogramData(histo HistogramData) {
	d.sampleCount += histo.sampleCount
	d.sampleSum += histo.sampleSum

	if len(histo.buckets) > 0 && d.buckets == nil {
		d.buckets = map[float64]uint64{}
	}

	for bound, count := range histo.buckets {
		// we assume that all histograms have same buckets
		d.buckets[bound] += count
	}
}

// Return prometheus metric from this histogram data.
func (d *HistogramData) Metric(desc *prometheus.Desc, labelValues ...string) prometheus.Metric {
	return prometheus.MustNewConstHistogram(desc, d.sampleCount, d.sampleSum, d.buckets, labelValues...)
}

// Creates new histogram data collector.
func NewHistogramDataCollector(desc *prometheus.Desc) *HistogramDataCollector {
	return &HistogramDataCollector{
		desc: desc,
		data: &HistogramData{},
	}
}

// HistogramDataCollector combines histogram data, with prometheus descriptor. It can be registered
// into prometheus to report histogram with stored data. Data can be updated via Add method.
type HistogramDataCollector struct {
	desc *prometheus.Desc

	dataMu sync.RWMutex
	data   *HistogramData
}

func (h *HistogramDataCollector) Describe(out chan<- *prometheus.Desc) {
	out <- h.desc
}

func (h *HistogramDataCollector) Collect(out chan<- prometheus.Metric) {
	h.dataMu.RLock()
	defer h.dataMu.RUnlock()

	out <- h.data.Metric(h.desc)
}

func (h *HistogramDataCollector) Add(hd HistogramData) {
	h.dataMu.Lock()
	defer h.dataMu.Unlock()

	h.data.AddHistogramData(hd)
}

// MetricFamiliesPerUser is a collection of metrics gathered via calling Gatherer.Gather() method on different
// gatherers, one per user.
type MetricFamiliesPerUser []struct {
	user    string
	metrics MetricFamilyMap
}

func (d MetricFamiliesPerUser) GetSumOfCounters(counter string) float64 {
	result := float64(0)
	for _, userEntry := range d {
		result += userEntry.metrics.SumCounters(counter)
	}
	return result
}

func (d MetricFamiliesPerUser) SendSumOfCounters(out chan<- prometheus.Metric, desc *prometheus.Desc, counter string) {
	out <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, d.GetSumOfCounters(counter))
}

func (d MetricFamiliesPerUser) SendSumOfCountersWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, counter string, labelNames ...string) {
	d.sumOfSingleValuesWithLabels(counter, counterValue, labelNames).WriteToMetricChannel(out, desc, prometheus.CounterValue)
}

func (d MetricFamiliesPerUser) SendSumOfCountersPerUser(out chan<- prometheus.Metric, desc *prometheus.Desc, counter string) {
	d.SendSumOfCountersPerUserWithLabels(out, desc, counter)
}

// SendSumOfCountersPerUserWithLabels provides metrics with the provided label names on a per-user basis. This function assumes that `user` is the
// first label on the provided metric Desc
func (d MetricFamiliesPerUser) SendSumOfCountersPerUserWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, metric string, labelNames ...string) {
	for _, userEntry := range d {
		if userEntry.user == "" {
			continue
		}

		result := singleValueWithLabelsMap{}
		userEntry.metrics.sumOfSingleValuesWithLabels(metric, labelNames, counterValue, result.aggregateFn)
		result.prependUserLabelValue(userEntry.user)
		result.WriteToMetricChannel(out, desc, prometheus.CounterValue)
	}
}

func (d MetricFamiliesPerUser) GetSumOfGauges(gauge string) float64 {
	result := float64(0)
	for _, userEntry := range d {
		result += userEntry.metrics.SumGauges(gauge)
	}
	return result
}

func (d MetricFamiliesPerUser) SendSumOfGauges(out chan<- prometheus.Metric, desc *prometheus.Desc, gauge string) {
	out <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, d.GetSumOfGauges(gauge))
}

func (d MetricFamiliesPerUser) SendSumOfGaugesWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, gauge string, labelNames ...string) {
	d.sumOfSingleValuesWithLabels(gauge, gaugeValue, labelNames).WriteToMetricChannel(out, desc, prometheus.GaugeValue)
}

func (d MetricFamiliesPerUser) SendSumOfGaugesPerUser(out chan<- prometheus.Metric, desc *prometheus.Desc, gauge string) {
	d.SendSumOfGaugesPerUserWithLabels(out, desc, gauge)
}

// SendSumOfGaugesPerUserWithLabels provides metrics with the provided label names on a per-user basis. This function assumes that `user` is the
// first label on the provided metric Desc
func (d MetricFamiliesPerUser) SendSumOfGaugesPerUserWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, metric string, labelNames ...string) {
	for _, userEntry := range d {
		if userEntry.user == "" {
			continue
		}

		result := singleValueWithLabelsMap{}
		userEntry.metrics.sumOfSingleValuesWithLabels(metric, labelNames, gaugeValue, result.aggregateFn)
		result.prependUserLabelValue(userEntry.user)
		result.WriteToMetricChannel(out, desc, prometheus.GaugeValue)
	}
}

func (d MetricFamiliesPerUser) sumOfSingleValuesWithLabels(metric string, fn func(*dto.Metric) float64, labelNames []string) singleValueWithLabelsMap {
	result := singleValueWithLabelsMap{}
	for _, userEntry := range d {
		userEntry.metrics.sumOfSingleValuesWithLabels(metric, labelNames, fn, result.aggregateFn)
	}
	return result
}

func (d MetricFamiliesPerUser) SendMaxOfGauges(out chan<- prometheus.Metric, desc *prometheus.Desc, gauge string) {
	result := math.NaN()
	for _, userEntry := range d {
		if value := userEntry.metrics.MaxGauges(gauge); math.IsNaN(result) || value > result {
			result = value
		}
	}

	// If there's no metric, we do send 0 which is the gauge default.
	if math.IsNaN(result) {
		result = 0
	}

	out <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, result)
}

func (d MetricFamiliesPerUser) SendMaxOfGaugesPerUser(out chan<- prometheus.Metric, desc *prometheus.Desc, gauge string) {
	for _, userEntry := range d {
		if userEntry.user == "" {
			continue
		}

		result := userEntry.metrics.MaxGauges(gauge)
		out <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, result, userEntry.user)
	}
}

func (d MetricFamiliesPerUser) SendSumOfSummaries(out chan<- prometheus.Metric, desc *prometheus.Desc, summaryName string) {
	summaryData := SummaryData{}
	for _, userEntry := range d {
		userEntry.metrics.SumSummariesTo(summaryName, &summaryData)
	}
	out <- summaryData.Metric(desc)
}

func (d MetricFamiliesPerUser) SendSumOfSummariesWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, summaryName string, labelNames ...string) {
	type summaryResult struct {
		data        SummaryData
		labelValues []string
	}

	result := map[string]summaryResult{}

	for _, mfm := range d {
		metricsPerLabelValue := getMetricsWithLabelNames(mfm.metrics[summaryName], labelNames)

		for key, mwl := range metricsPerLabelValue {
			for _, m := range mwl.metrics {
				r := result[key]
				if r.labelValues == nil {
					r.labelValues = mwl.labelValues
				}

				r.data.AddSummary(m.GetSummary())
				result[key] = r
			}
		}
	}

	for _, sr := range result {
		out <- sr.data.Metric(desc, sr.labelValues...)
	}
}

func (d MetricFamiliesPerUser) SendSumOfSummariesPerUser(out chan<- prometheus.Metric, desc *prometheus.Desc, summaryName string) {
	for _, userEntry := range d {
		if userEntry.user == "" {
			continue
		}

		data := userEntry.metrics.SumSummaries(summaryName)
		out <- data.Metric(desc, userEntry.user)
	}
}

func (d MetricFamiliesPerUser) SendSumOfHistograms(out chan<- prometheus.Metric, desc *prometheus.Desc, histogramName string) {
	hd := HistogramData{}
	for _, userEntry := range d {
		userEntry.metrics.SumHistogramsTo(histogramName, &hd)
	}
	out <- hd.Metric(desc)
}

func (d MetricFamiliesPerUser) SendSumOfHistogramsWithLabels(out chan<- prometheus.Metric, desc *prometheus.Desc, histogramName string, labelNames ...string) {
	type histogramResult struct {
		data        HistogramData
		labelValues []string
	}

	result := map[string]histogramResult{}

	for _, mfm := range d {
		metricsPerLabelValue := getMetricsWithLabelNames(mfm.metrics[histogramName], labelNames)

		for key, mwl := range metricsPerLabelValue {
			for _, m := range mwl.metrics {
				r := result[key]
				if r.labelValues == nil {
					r.labelValues = mwl.labelValues
				}

				r.data.AddHistogram(m.GetHistogram())
				result[key] = r
			}
		}
	}

	for _, hg := range result {
		out <- hg.data.Metric(desc, hg.labelValues...)
	}
}

// Data for single value (counter/gauge) with labels.
type singleValueWithLabels struct {
	Value       float64
	LabelValues []string
}

// Key to this map is value unique to label values (generated by getLabelsString function)
type singleValueWithLabelsMap map[string]singleValueWithLabels

// This function is used to aggregate results with different labels into a map. Values for same labels are added together.
func (m singleValueWithLabelsMap) aggregateFn(labelsKey string, labelValues []string, value float64) {
	r := m[labelsKey]
	if r.LabelValues == nil {
		r.LabelValues = labelValues
	}

	r.Value += value
	m[labelsKey] = r
}

func (m singleValueWithLabelsMap) prependUserLabelValue(user string) {
	for key, mlv := range m {
		mlv.LabelValues = append([]string{user}, mlv.LabelValues...)
		m[key] = mlv
	}
}

func (m singleValueWithLabelsMap) WriteToMetricChannel(out chan<- prometheus.Metric, desc *prometheus.Desc, valueType prometheus.ValueType) {
	for _, cr := range m {
		out <- prometheus.MustNewConstMetric(desc, valueType, cr.Value, cr.LabelValues...)
	}
}

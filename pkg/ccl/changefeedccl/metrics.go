// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcutils"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/kvevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/schemafeed"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/multitenant"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/metric/aggmetric"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	changefeedCheckpointHistMaxLatency = 30 * time.Second
	changefeedBatchHistMaxLatency      = 30 * time.Second
	changefeedFlushHistMaxLatency      = 1 * time.Minute
	changefeedIOQueueMaxLatency        = 5 * time.Minute
	admitLatencyMaxValue               = 1 * time.Minute
	commitLatencyMaxValue              = 10 * time.Minute
)

// max length for the scope name.
const maxSLIScopeNameLen = 128

// defaultSLIScope is the name of the default SLI scope -- i.e. the set of metrics
// keeping track of all changefeeds which did not have explicit sli scope specified.
const defaultSLIScope = "default"

// AggMetrics are aggregated metrics keeping track of aggregated changefeed performance
// indicators, combined with a limited number of per-changefeed indicators.
type AggMetrics struct {
	EmittedMessages           *aggmetric.AggCounter
	FilteredMessages          *aggmetric.AggCounter
	MessageSize               *aggmetric.AggHistogram
	EmittedBytes              *aggmetric.AggCounter
	FlushedBytes              *aggmetric.AggCounter
	BatchHistNanos            *aggmetric.AggHistogram
	Flushes                   *aggmetric.AggCounter
	FlushHistNanos            *aggmetric.AggHistogram
	SizeBasedFlushes          *aggmetric.AggCounter
	ParallelIOQueueNanos      *aggmetric.AggHistogram
	SinkIOInflight            *aggmetric.AggGauge
	CommitLatency             *aggmetric.AggHistogram
	BackfillCount             *aggmetric.AggGauge
	BackfillPendingRanges     *aggmetric.AggGauge
	ErrorRetries              *aggmetric.AggCounter
	AdmitLatency              *aggmetric.AggHistogram
	RunningCount              *aggmetric.AggGauge
	BatchReductionCount       *aggmetric.AggGauge
	InternalRetryMessageCount *aggmetric.AggGauge
	SchemaRegistrations       *aggmetric.AggCounter
	SchemaRegistryRetries     *aggmetric.AggCounter
	AggregatorProgress        *aggmetric.AggGauge
	CheckpointProgress        *aggmetric.AggGauge
	LaggingRanges             *aggmetric.AggGauge

	// There is always at least 1 sliMetrics created for defaultSLI scope.
	mu struct {
		syncutil.Mutex
		sliMetrics map[string]*sliMetrics
	}
}

const (
	requiresResourceAccounting = true
	noResourceAccounting       = false
)

type metricsRecorderBuilder func(requiresCostAccounting bool) metricsRecorder

var nilMetricsRecorderBuilder metricsRecorderBuilder = func(_ bool) metricsRecorder { return (*sliMetrics)(nil) }

type metricsRecorder interface {
	recordMessageSize(int64)
	recordInternalRetry(int64, bool)
	recordOneMessage() recordOneMessageCallback
	recordEmittedBatch(startTime time.Time, numMessages int, mvcc hlc.Timestamp, bytes int, compressedBytes int)
	recordResolvedCallback() func()
	recordFlushRequestCallback() func()
	getBackfillCallback() func() func()
	getBackfillRangeCallback() func(int64) (func(), func())
	recordSizeBasedFlush()
	recordParallelIOQueueLatency(time.Duration)
	recordSinkIOInflightChange(int64)
}

var _ metricsRecorder = (*sliMetrics)(nil)
var _ metricsRecorder = (*wrappingCostController)(nil)

// MetricStruct implements metric.Struct interface.
func (a *AggMetrics) MetricStruct() {}

// sliMetrics holds all SLI related metrics aggregated into AggMetrics.
type sliMetrics struct {
	EmittedMessages           *aggmetric.Counter
	FilteredMessages          *aggmetric.Counter
	MessageSize               *aggmetric.Histogram
	EmittedBytes              *aggmetric.Counter
	FlushedBytes              *aggmetric.Counter
	BatchHistNanos            *aggmetric.Histogram
	Flushes                   *aggmetric.Counter
	FlushHistNanos            *aggmetric.Histogram
	SizeBasedFlushes          *aggmetric.Counter
	ParallelIOQueueNanos      *aggmetric.Histogram
	SinkIOInflight            *aggmetric.Gauge
	CommitLatency             *aggmetric.Histogram
	ErrorRetries              *aggmetric.Counter
	AdmitLatency              *aggmetric.Histogram
	BackfillCount             *aggmetric.Gauge
	BackfillPendingRanges     *aggmetric.Gauge
	RunningCount              *aggmetric.Gauge
	BatchReductionCount       *aggmetric.Gauge
	InternalRetryMessageCount *aggmetric.Gauge
	SchemaRegistrations       *aggmetric.Counter
	SchemaRegistryRetries     *aggmetric.Counter
	AggregatorProgress        *aggmetric.Gauge
	CheckpointProgress        *aggmetric.Gauge
	LaggingRanges             *aggmetric.Gauge

	mu struct {
		syncutil.Mutex
		id         int64
		resolved   map[int64]hlc.Timestamp
		checkpoint map[int64]hlc.Timestamp
	}
}

// closeId unregisters an id. The id can still be used after its closed, but
// such usages will be noops.
func (m *sliMetrics) closeId(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mu.checkpoint, id)
	delete(m.mu.resolved, id)
}

// setResolved writes a resolved timestamp entry for the given id.
func (m *sliMetrics) setResolved(id int64, ts hlc.Timestamp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mu.resolved[id]; ok {
		m.mu.resolved[id] = ts
	}
}

// setCheckpoint writes a checkpoint timestamp entry for the given id.
func (m *sliMetrics) setCheckpoint(id int64, ts hlc.Timestamp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mu.checkpoint[id]; ok {
		m.mu.checkpoint[id] = ts
	}
}

// claimId claims a unique ID.
func (m *sliMetrics) claimId() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.mu.id
	// Seed entries with the zero timestamp and expect these to be
	// ignored until a nonzero timestamp is written.
	m.mu.checkpoint[id] = hlc.Timestamp{}
	m.mu.resolved[id] = hlc.Timestamp{}
	m.mu.id++
	return id
}

// sinkDoesNotCompress is a sentinel value indicating the sink
// does not compress the data it emits.
const sinkDoesNotCompress = -1

type recordOneMessageCallback func(mvcc hlc.Timestamp, bytes int, compressedBytes int)

func (m *sliMetrics) recordOneMessage() recordOneMessageCallback {
	if m == nil {
		return func(mvcc hlc.Timestamp, bytes int, compressedBytes int) {}
	}

	start := timeutil.Now()
	return func(mvcc hlc.Timestamp, bytes int, compressedBytes int) {
		m.MessageSize.RecordValue(int64(bytes))
		m.recordEmittedBatch(start, 1, mvcc, bytes, compressedBytes)
	}
}

func (m *sliMetrics) recordMessageSize(sz int64) {
	if m != nil {
		m.MessageSize.RecordValue(sz)
	}
}

func (m *sliMetrics) recordInternalRetry(numMessages int64, reducedBatchSize bool) {
	if m == nil {
		return
	}

	if reducedBatchSize {
		m.BatchReductionCount.Inc(1)
	}

	m.InternalRetryMessageCount.Inc(numMessages)
}

func (m *sliMetrics) recordEmittedBatch(
	startTime time.Time, numMessages int, mvcc hlc.Timestamp, bytes int, compressedBytes int,
) {
	if m == nil {
		return
	}
	emitNanos := timeutil.Since(startTime).Nanoseconds()
	m.EmittedMessages.Inc(int64(numMessages))
	m.EmittedBytes.Inc(int64(bytes))
	if compressedBytes == sinkDoesNotCompress {
		compressedBytes = bytes
	}
	m.FlushedBytes.Inc(int64(compressedBytes))
	m.BatchHistNanos.RecordValue(emitNanos)
	if m.BackfillCount.Value() == 0 {
		m.CommitLatency.RecordValue(timeutil.Since(mvcc.GoTime()).Nanoseconds())
	}
}

func (m *sliMetrics) recordResolvedCallback() func() {
	if m == nil {
		return func() {}
	}

	start := timeutil.Now()
	return func() {
		emitNanos := timeutil.Since(start).Nanoseconds()
		m.EmittedMessages.Inc(1)
		m.BatchHistNanos.RecordValue(emitNanos)
	}
}

func (m *sliMetrics) recordFlushRequestCallback() func() {
	if m == nil {
		return func() {}
	}

	start := timeutil.Now()
	return func() {
		flushNanos := timeutil.Since(start).Nanoseconds()
		m.Flushes.Inc(1)
		m.FlushHistNanos.RecordValue(flushNanos)
	}
}

func (m *sliMetrics) getBackfillCallback() func() func() {
	return func() func() {
		m.BackfillCount.Inc(1)
		return func() {
			m.BackfillCount.Dec(1)
		}
	}
}

// getBackfillRangeCallback returns a backfillRangeCallback that is to be called
// at the beginning of a backfill with the number of ranges that will be scanned
// and returns a two callbacks to decrement the value until all ranges have
// been emitted or clear the number completely if the backfill is cancelled.
// Note: dec() should only be called as many times as the initial value, and
// clear() should be called when there will never be another dec() call.
func (m *sliMetrics) getBackfillRangeCallback() func(int64) (func(), func()) {
	return func(initial int64) (dec func(), clear func()) {
		remaining := initial
		m.BackfillPendingRanges.Inc(initial)
		dec = func() {
			m.BackfillPendingRanges.Dec(1)
			atomic.AddInt64(&remaining, -1)
		}
		clear = func() {
			m.BackfillPendingRanges.Dec(remaining)
			atomic.AddInt64(&remaining, -remaining)
		}
		return
	}
}

// Record size-based flush.
func (m *sliMetrics) recordSizeBasedFlush() {
	if m == nil {
		return
	}

	m.SizeBasedFlushes.Inc(1)
}

func (m *sliMetrics) recordParallelIOQueueLatency(latency time.Duration) {
	if m == nil {
		return
	}
	m.ParallelIOQueueNanos.RecordValue(latency.Nanoseconds())
}
func (m *sliMetrics) recordSinkIOInflightChange(delta int64) {
	if m == nil {
		return
	}

	m.SinkIOInflight.Inc(delta)
}

type wrappingCostController struct {
	ctx      context.Context
	inner    metricsRecorder
	recorder multitenant.TenantSideExternalIORecorder
}

func maybeWrapMetrics(
	ctx context.Context, inner metricsRecorder, recorder multitenant.TenantSideExternalIORecorder,
) metricsRecorder {
	if recorder == nil {
		return inner
	}
	return &wrappingCostController{ctx: ctx, inner: inner, recorder: recorder}
}

func (w *wrappingCostController) recordExternalIO(bytes int, compressedBytes int) {
	if compressedBytes == sinkDoesNotCompress {
		compressedBytes = bytes
	}
	// NB: We don't Wait for RUs for changefeeds; but, this call may put the RU limiter in debt which
	// will impact future KV requests.
	w.recorder.OnExternalIO(w.ctx, multitenant.ExternalIOUsage{EgressBytes: int64(compressedBytes)})
}

func (w *wrappingCostController) recordOneMessage() recordOneMessageCallback {
	innerCallback := w.inner.recordOneMessage()
	return func(mvcc hlc.Timestamp, bytes int, compressedBytes int) {
		w.recordExternalIO(bytes, compressedBytes)
		innerCallback(mvcc, bytes, compressedBytes)
	}
}

func (w *wrappingCostController) recordEmittedBatch(
	startTime time.Time, numMessages int, mvcc hlc.Timestamp, bytes int, compressedBytes int,
) {
	w.recordExternalIO(bytes, compressedBytes)
	w.inner.recordEmittedBatch(startTime, numMessages, mvcc, bytes, compressedBytes)
}

func (w *wrappingCostController) recordMessageSize(sz int64) {
	w.inner.recordMessageSize(sz)
}

func (w *wrappingCostController) recordInternalRetry(numMessages int64, reducedBatchSize bool) {
	w.inner.recordInternalRetry(numMessages, reducedBatchSize)
}

func (w *wrappingCostController) recordResolvedCallback() func() {
	// TODO(ssd): We don't count resolved messages currently. These messages should be relatively
	// small and the error here is further in the favor of the user.
	return w.inner.recordResolvedCallback()
}

func (w *wrappingCostController) recordFlushRequestCallback() func() {
	return w.inner.recordFlushRequestCallback()
}

func (w *wrappingCostController) getBackfillCallback() func() func() {
	return w.inner.getBackfillCallback()
}

func (w *wrappingCostController) getBackfillRangeCallback() func(int64) (func(), func()) {
	return w.inner.getBackfillRangeCallback()
}

// Record size-based flush.
func (w *wrappingCostController) recordSizeBasedFlush() {
	w.inner.recordSizeBasedFlush()
}

func (w *wrappingCostController) recordParallelIOQueueLatency(latency time.Duration) {
	w.inner.recordParallelIOQueueLatency(latency)
}

func (w *wrappingCostController) recordSinkIOInflightChange(delta int64) {
	w.inner.recordSinkIOInflightChange(delta)
}

var (
	metaChangefeedForwardedResolvedMessages = metric.Metadata{
		Name:        "changefeed.forwarded_resolved_messages",
		Help:        "Resolved timestamps forwarded from the change aggregator to the change frontier",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedErrorRetries = metric.Metadata{
		Name:        "changefeed.error_retries",
		Help:        "Total retryable errors encountered by all changefeeds",
		Measurement: "Errors",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedFailures = metric.Metadata{
		Name:        "changefeed.failures",
		Help:        "Total number of changefeed jobs which have failed",
		Measurement: "Errors",
		Unit:        metric.Unit_COUNT,
	}

	metaEventQueueTime = metric.Metadata{
		Name:        "changefeed.queue_time_nanos",
		Help:        "Time KV event spent waiting to be processed",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}

	metaChangefeedCheckpointHistNanos = metric.Metadata{
		Name:        "changefeed.checkpoint_hist_nanos",
		Help:        "Time spent checkpointing changefeed progress",
		Measurement: "Changefeeds",
		Unit:        metric.Unit_NANOSECONDS,
	}

	// TODO(dan): This was intended to be a measure of the minimum distance of
	// any changefeed ahead of its gc ttl threshold, but keeping that correct in
	// the face of changing zone configs is much harder, so this will have to do
	// for now.
	metaChangefeedMaxBehindNanos = metric.Metadata{
		Name:        "changefeed.max_behind_nanos",
		Help:        "(Deprecated in favor of checkpoint_progress) The most any changefeed's persisted checkpoint is behind the present",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}

	metaChangefeedFrontierUpdates = metric.Metadata{
		Name:        "changefeed.frontier_updates",
		Help:        "Number of change frontier updates across all feeds",
		Measurement: "Updates",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedEventConsumerFlushNanos = metric.Metadata{
		Name:        "changefeed.nprocs_flush_nanos",
		Help:        "Total time spent idle waiting for the parallel consumer to flush",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaChangefeedEventConsumerConsumeNanos = metric.Metadata{
		Name:        "changefeed.nprocs_consume_event_nanos",
		Help:        "Total time spent waiting to add an event to the parallel consumer",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaChangefeedEventConsumerInFlightEvents = metric.Metadata{
		Name:        "changefeed.nprocs_in_flight_count",
		Help:        "Number of buffered events in the parallel consumer",
		Measurement: "Count of Events",
		Unit:        metric.Unit_COUNT,
	}
)

func newAggregateMetrics(histogramWindow time.Duration) *AggMetrics {
	metaChangefeedEmittedMessages := metric.Metadata{
		Name:        "changefeed.emitted_messages",
		Help:        "Messages emitted by all feeds",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedFilteredMessages := metric.Metadata{
		Name: "changefeed.filtered_messages",
		Help: "Messages filtered out by all feeds. " +
			"This count does not include the number of messages that may be filtered " +
			"due to the range constraints.",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedEmittedBytes := metric.Metadata{
		Name:        "changefeed.emitted_bytes",
		Help:        "Bytes emitted by all feeds",
		Measurement: "Bytes",
		Unit:        metric.Unit_BYTES,
	}
	metaChangefeedFlushedBytes := metric.Metadata{
		Name:        "changefeed.flushed_bytes",
		Help:        "Bytes emitted by all feeds; maybe different from changefeed.emitted_bytes when compression is enabled",
		Measurement: "Bytes",
		Unit:        metric.Unit_BYTES,
	}
	metaChangefeedFlushes := metric.Metadata{
		Name:        "changefeed.flushes",
		Help:        "Total flushes across all feeds",
		Measurement: "Flushes",
		Unit:        metric.Unit_COUNT,
	}
	metaSizeBasedFlushes := metric.Metadata{
		Name:        "changefeed.size_based_flushes",
		Help:        "Total size based flushes across all feeds",
		Measurement: "Flushes",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedBatchHistNanos := metric.Metadata{
		Name:        "changefeed.sink_batch_hist_nanos",
		Help:        "Time spent batched in the sink buffer before being flushed and acknowledged",
		Measurement: "Changefeeds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaChangefeedFlushHistNanos := metric.Metadata{
		Name:        "changefeed.flush_hist_nanos",
		Help:        "Time spent flushing messages across all changefeeds",
		Measurement: "Changefeeds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaCommitLatency := metric.Metadata{
		Name: "changefeed.commit_latency",
		Help: "Event commit latency: a difference between event MVCC timestamp " +
			"and the time it was acknowledged by the downstream sink.  If the sink batches events, " +
			" then the difference between the oldest event in the batch and acknowledgement is recorded; " +
			"Excludes latency during backfill",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaAdmitLatency := metric.Metadata{
		Name: "changefeed.admit_latency",
		Help: "Event admission latency: a difference between event MVCC timestamp " +
			"and the time it was admitted into changefeed pipeline; " +
			"Note: this metric includes the time spent waiting until event can be processed due " +
			"to backpressure or time spent resolving schema descriptors. " +
			"Also note, this metric excludes latency during backfill",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaChangefeedBackfillCount := metric.Metadata{
		Name:        "changefeed.backfill_count",
		Help:        "Number of changefeeds currently executing backfill",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedBackfillPendingRanges := metric.Metadata{
		Name:        "changefeed.backfill_pending_ranges",
		Help:        "Number of ranges in an ongoing backfill that are yet to be fully emitted",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedRunning := metric.Metadata{
		Name:        "changefeed.running",
		Help:        "Number of currently running changefeeds, including sinkless",
		Measurement: "Changefeeds",
		Unit:        metric.Unit_COUNT,
	}
	metaMessageSize := metric.Metadata{
		Name:        "changefeed.message_size_hist",
		Help:        "Message size histogram",
		Measurement: "Bytes",
		Unit:        metric.Unit_BYTES,
	}
	metaBatchReductionCount := metric.Metadata{
		Name:        "changefeed.batch_reduction_count",
		Help:        "Number of times a changefeed aggregator node attempted to reduce the size of message batches it emitted to the sink",
		Measurement: "Batch Size Reductions",
		Unit:        metric.Unit_COUNT,
	}
	metaInternalRetryMessageCount := metric.Metadata{
		Name:        "changefeed.internal_retry_message_count",
		Help:        "Number of messages for which an attempt to retry them within an aggregator node was made",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaSchemaRegistryRetriesCount := metric.Metadata{
		Name:        "changefeed.schema_registry.retry_count",
		Help:        "Number of retries encountered when sending requests to the schema registry",
		Measurement: "Retries",
		Unit:        metric.Unit_COUNT,
	}
	metaSchemaRegistryRegistrations := metric.Metadata{
		Name:        "changefeed.schema_registry.registrations",
		Help:        "Number of registration attempts with the schema registry",
		Measurement: "Registrations",
		Unit:        metric.Unit_COUNT,
	}
	metaChangefeedParallelIOQueueNanos := metric.Metadata{
		Name:        "changefeed.parallel_io_queue_nanos",
		Help:        "Time spent with outgoing requests to the sink waiting in queue due to inflight requests with conflicting keys",
		Measurement: "Changefeeds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	metaChangefeedSinkIOInflight := metric.Metadata{
		Name:        "changefeed.sink_io_inflight",
		Help:        "The number of keys currently inflight as IO requests being sent to the sink",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaAggregatorProgress := metric.Metadata{
		Name:        "changefeed.aggregator_progress",
		Help:        "The earliest timestamp up to which any aggregator is guaranteed to have emitted all values for",
		Measurement: "Unix Timestamp Nanoseconds",
		Unit:        metric.Unit_TIMESTAMP_NS,
	}
	metaCheckpointProgress := metric.Metadata{
		Name:        "changefeed.checkpoint_progress",
		Help:        "The earliest timestamp of any changefeed's persisted checkpoint (values prior to this timestamp will never need to be re-emitted)",
		Measurement: "Unix Timestamp Nanoseconds",
		Unit:        metric.Unit_TIMESTAMP_NS,
	}
	metaLaggingRangePercentage := metric.Metadata{
		Name:        "changefeed.lagging_ranges",
		Help:        "The number of ranges considered to be lagging behind",
		Measurement: "Ranges",
		Unit:        metric.Unit_COUNT,
	}

	functionalGaugeMinFn := func(childValues []int64) int64 {
		var min int64
		for _, val := range childValues {
			if min == 0 || (val != 0 && val < min) {
				min = val
			}
		}
		return min
	}

	// NB: When adding new histograms, use sigFigs = 1.  Older histograms
	// retain significant figures of 2.
	b := aggmetric.MakeBuilder("scope")
	a := &AggMetrics{
		ErrorRetries:     b.Counter(metaChangefeedErrorRetries),
		EmittedMessages:  b.Counter(metaChangefeedEmittedMessages),
		FilteredMessages: b.Counter(metaChangefeedFilteredMessages),
		MessageSize: b.Histogram(metric.HistogramOptions{
			Metadata:     metaMessageSize,
			Duration:     histogramWindow,
			MaxVal:       10 << 20, /* 10MB max message size */
			SigFigs:      1,
			BucketConfig: metric.DataSize16MBBuckets,
		}),
		EmittedBytes:     b.Counter(metaChangefeedEmittedBytes),
		FlushedBytes:     b.Counter(metaChangefeedFlushedBytes),
		Flushes:          b.Counter(metaChangefeedFlushes),
		SizeBasedFlushes: b.Counter(metaSizeBasedFlushes),
		ParallelIOQueueNanos: b.Histogram(metric.HistogramOptions{
			Metadata:     metaChangefeedParallelIOQueueNanos,
			Duration:     histogramWindow,
			MaxVal:       changefeedIOQueueMaxLatency.Nanoseconds(),
			SigFigs:      2,
			BucketConfig: metric.BatchProcessLatencyBuckets,
		}),
		SinkIOInflight: b.Gauge(metaChangefeedSinkIOInflight),

		BatchHistNanos: b.Histogram(metric.HistogramOptions{
			Metadata:     metaChangefeedBatchHistNanos,
			Duration:     histogramWindow,
			MaxVal:       changefeedBatchHistMaxLatency.Nanoseconds(),
			SigFigs:      1,
			BucketConfig: metric.BatchProcessLatencyBuckets,
		}),
		FlushHistNanos: b.Histogram(metric.HistogramOptions{
			Metadata:     metaChangefeedFlushHistNanos,
			Duration:     histogramWindow,
			MaxVal:       changefeedFlushHistMaxLatency.Nanoseconds(),
			SigFigs:      2,
			BucketConfig: metric.BatchProcessLatencyBuckets,
		}),
		CommitLatency: b.Histogram(metric.HistogramOptions{
			Metadata:     metaCommitLatency,
			Duration:     histogramWindow,
			MaxVal:       commitLatencyMaxValue.Nanoseconds(),
			SigFigs:      1,
			BucketConfig: metric.BatchProcessLatencyBuckets,
		}),
		AdmitLatency: b.Histogram(metric.HistogramOptions{
			Metadata:     metaAdmitLatency,
			Duration:     histogramWindow,
			MaxVal:       admitLatencyMaxValue.Nanoseconds(),
			SigFigs:      1,
			BucketConfig: metric.BatchProcessLatencyBuckets,
		}),
		BackfillCount:             b.Gauge(metaChangefeedBackfillCount),
		BackfillPendingRanges:     b.Gauge(metaChangefeedBackfillPendingRanges),
		RunningCount:              b.Gauge(metaChangefeedRunning),
		BatchReductionCount:       b.Gauge(metaBatchReductionCount),
		InternalRetryMessageCount: b.Gauge(metaInternalRetryMessageCount),
		SchemaRegistryRetries:     b.Counter(metaSchemaRegistryRetriesCount),
		SchemaRegistrations:       b.Counter(metaSchemaRegistryRegistrations),
		AggregatorProgress:        b.FunctionalGauge(metaAggregatorProgress, functionalGaugeMinFn),
		CheckpointProgress:        b.FunctionalGauge(metaCheckpointProgress, functionalGaugeMinFn),
		LaggingRanges:             b.Gauge(metaLaggingRangePercentage),
	}
	a.mu.sliMetrics = make(map[string]*sliMetrics)
	_, err := a.getOrCreateScope(defaultSLIScope)
	if err != nil {
		// defaultSLIScope must always exist.
		panic(err)
	}
	return a
}

func (a *AggMetrics) getOrCreateScope(scope string) (*sliMetrics, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	scope = strings.TrimSpace(strings.ToLower(scope))

	if scope == "" {
		scope = defaultSLIScope
	}

	if len(scope) > maxSLIScopeNameLen {
		return nil, pgerror.Newf(pgcode.ConfigurationLimitExceeded,
			"scope name length must be less than %d bytes", maxSLIScopeNameLen)
	}

	if s, ok := a.mu.sliMetrics[scope]; ok {
		return s, nil
	}

	if scope != defaultSLIScope {
		const failSafeMax = 1024
		if len(a.mu.sliMetrics) == failSafeMax {
			return nil, pgerror.Newf(pgcode.ConfigurationLimitExceeded,
				"too many metrics labels; max %d", failSafeMax)
		}
	}

	sm := &sliMetrics{
		EmittedMessages:           a.EmittedMessages.AddChild(scope),
		FilteredMessages:          a.FilteredMessages.AddChild(scope),
		MessageSize:               a.MessageSize.AddChild(scope),
		EmittedBytes:              a.EmittedBytes.AddChild(scope),
		FlushedBytes:              a.FlushedBytes.AddChild(scope),
		BatchHistNanos:            a.BatchHistNanos.AddChild(scope),
		Flushes:                   a.Flushes.AddChild(scope),
		FlushHistNanos:            a.FlushHistNanos.AddChild(scope),
		SizeBasedFlushes:          a.SizeBasedFlushes.AddChild(scope),
		ParallelIOQueueNanos:      a.ParallelIOQueueNanos.AddChild(scope),
		SinkIOInflight:            a.SinkIOInflight.AddChild(scope),
		CommitLatency:             a.CommitLatency.AddChild(scope),
		ErrorRetries:              a.ErrorRetries.AddChild(scope),
		AdmitLatency:              a.AdmitLatency.AddChild(scope),
		BackfillCount:             a.BackfillCount.AddChild(scope),
		BackfillPendingRanges:     a.BackfillPendingRanges.AddChild(scope),
		RunningCount:              a.RunningCount.AddChild(scope),
		BatchReductionCount:       a.BatchReductionCount.AddChild(scope),
		InternalRetryMessageCount: a.InternalRetryMessageCount.AddChild(scope),
		SchemaRegistryRetries:     a.SchemaRegistryRetries.AddChild(scope),
		SchemaRegistrations:       a.SchemaRegistrations.AddChild(scope),
		LaggingRanges:             a.LaggingRanges.AddChild(scope),
	}
	sm.mu.resolved = make(map[int64]hlc.Timestamp)
	sm.mu.checkpoint = make(map[int64]hlc.Timestamp)
	sm.mu.id = 1 // start the first id at 1 so we can detect intiialization

	minTimestampGetter := func(m map[int64]hlc.Timestamp) func() int64 {
		return func() int64 {
			sm.mu.Lock()
			defer sm.mu.Unlock()
			var minTs int64
			for _, hlcTs := range m {
				// Ignore empty timestamps which new entries are seeded with.
				if hlcTs.WallTime != 0 {
					// Track the min timestamp.
					if minTs == 0 || hlcTs.WallTime < minTs {
						minTs = hlcTs.WallTime
					}
				}
			}
			return minTs
		}
	}
	sm.AggregatorProgress = a.AggregatorProgress.AddFunctionalChild(minTimestampGetter(sm.mu.resolved), scope)
	sm.CheckpointProgress = a.CheckpointProgress.AddFunctionalChild(minTimestampGetter(sm.mu.checkpoint), scope)

	a.mu.sliMetrics[scope] = sm
	return sm, nil
}

// getLaggingRangesCallback returns a function which can be called to update the
// lagging ranges metric. It should be called with the current number of lagging
// ranges.
func (s *sliMetrics) getLaggingRangesCallback() func(int64) {
	// Because this gauge is shared between changefeeds in the same metrics scope,
	// we must instead modify it using `Inc` and `Dec` (as opposed to `Update`) to
	// ensure values written by others are not overwritten. The code below is used
	// to determine the deltas based on the last known number of lagging ranges.
	//
	// Example:
	//
	// Initially there are 0 lagging ranges, so `last` is 0. Assume the gauge
	// has an arbitrary value X.
	//
	// If 10 ranges are behind, last=0,i=10: X.Dec(0 - 10) = X.Inc(10)
	// If 3 ranges catch up, last=10,i=7: X.Dec(10 - 7) = X.Dec(3)
	// If 4 ranges fall behind, last=7,i=11: X.Dec(7 - 11) = X.Inc(4)
	// If 1 lagging range is deleted, last=7,i=10: X.Dec(11-10) = X.Dec(1)
	last := struct {
		syncutil.Mutex
		v int64
	}{}
	return func(i int64) {
		last.Lock()
		defer last.Unlock()
		s.LaggingRanges.Dec(last.v - i)
		last.v = i
	}
}

// Metrics are for production monitoring of changefeeds.
type Metrics struct {
	AggMetrics                     *AggMetrics
	KVFeedMetrics                  kvevent.Metrics
	SchemaFeedMetrics              schemafeed.Metrics
	Failures                       *metric.Counter
	ResolvedMessages               *metric.Counter
	QueueTimeNanos                 *metric.Counter
	CheckpointHistNanos            metric.IHistogram
	FrontierUpdates                *metric.Counter
	ThrottleMetrics                cdcutils.Metrics
	ParallelConsumerFlushNanos     metric.IHistogram
	ParallelConsumerConsumeNanos   metric.IHistogram
	ParallelConsumerInFlightEvents *metric.Gauge

	// This map and the MaxBehindNanos metric are deprecated in favor of
	// CheckpointProgress which is stored in the sliMetrics.
	mu struct {
		syncutil.Mutex
		id       int
		resolved map[int]hlc.Timestamp
	}
	MaxBehindNanos *metric.Gauge
}

// MetricStruct implements the metric.Struct interface.
func (*Metrics) MetricStruct() {}

// getSLIMetrics returns SLIMeterics associated with the specified scope.
func (m *Metrics) getSLIMetrics(scope string) (*sliMetrics, error) {
	return m.AggMetrics.getOrCreateScope(scope)
}

// MakeMetrics makes the metrics for changefeed monitoring.
func MakeMetrics(histogramWindow time.Duration) metric.Struct {
	m := &Metrics{
		AggMetrics:        newAggregateMetrics(histogramWindow),
		KVFeedMetrics:     kvevent.MakeMetrics(histogramWindow),
		SchemaFeedMetrics: schemafeed.MakeMetrics(histogramWindow),
		ResolvedMessages:  metric.NewCounter(metaChangefeedForwardedResolvedMessages),
		Failures:          metric.NewCounter(metaChangefeedFailures),
		QueueTimeNanos:    metric.NewCounter(metaEventQueueTime),
		CheckpointHistNanos: metric.NewHistogram(metric.HistogramOptions{
			Metadata:     metaChangefeedCheckpointHistNanos,
			Duration:     histogramWindow,
			MaxVal:       changefeedCheckpointHistMaxLatency.Nanoseconds(),
			SigFigs:      2,
			BucketConfig: metric.IOLatencyBuckets,
		}),
		FrontierUpdates: metric.NewCounter(metaChangefeedFrontierUpdates),
		ThrottleMetrics: cdcutils.MakeMetrics(histogramWindow),
		// Below two metrics were never implemented using the hdr histogram. Set ForceUsePrometheus
		// to true.
		ParallelConsumerFlushNanos: metric.NewHistogram(metric.HistogramOptions{
			Metadata:     metaChangefeedEventConsumerFlushNanos,
			Duration:     histogramWindow,
			BucketConfig: metric.IOLatencyBuckets,
			Mode:         metric.HistogramModePrometheus,
		}),
		ParallelConsumerConsumeNanos: metric.NewHistogram(metric.HistogramOptions{
			Metadata:     metaChangefeedEventConsumerConsumeNanos,
			Duration:     histogramWindow,
			BucketConfig: metric.IOLatencyBuckets,
			Mode:         metric.HistogramModePrometheus,
		}),
		ParallelConsumerInFlightEvents: metric.NewGauge(metaChangefeedEventConsumerInFlightEvents),
	}

	m.mu.resolved = make(map[int]hlc.Timestamp)
	m.mu.id = 1 // start the first id at 1 so we can detect initialization
	m.MaxBehindNanos = metric.NewFunctionalGauge(metaChangefeedMaxBehindNanos, func() int64 {
		now := timeutil.Now()
		var maxBehind time.Duration
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, resolved := range m.mu.resolved {
			if behind := now.Sub(resolved.GoTime()); behind > maxBehind {
				maxBehind = behind
			}
		}
		return maxBehind.Nanoseconds()
	})
	return m
}

func init() {
	jobs.MakeChangefeedMetricsHook = MakeMetrics
}

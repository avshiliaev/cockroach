// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package admission

import (
	"context"
	"math"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/redact"
)

// MinFlushUtilizationFraction is a lower-bound on the dynamically adjusted
// flush utilization target fraction that attempts to reduce write stalls. Set
// it to a high fraction (>>1, e.g. 10), to effectively disable flush based
// tokens.
//
// The target fraction is used to multiply the (measured) peak flush rate, to
// compute the flush tokens. For example, if the dynamic target fraction (for
// which this setting provides a lower bound) is currently 0.75, then
// 0.75*peak-flush-rate will be used to set the flush tokens. The lower bound
// of 0.5 should not need to be tuned, and should not be tuned without
// consultation with a domain expert. If the storage.write-stall-nanos
// indicates significant write stalls, and the granter logs show that the
// dynamic target fraction has already reached the lower bound, one can
// consider lowering it slightly and then observe the effect.
var MinFlushUtilizationFraction = settings.RegisterFloatSetting(
	settings.SystemOnly,
	"admission.min_flush_util_fraction",
	"when computing flush tokens, this fraction is a lower bound on the dynamically "+
		"adjusted flush utilization target fraction that attempts to reduce write stalls. Set "+
		"it to a high fraction (>>1, e.g. 10), to disable flush based tokens. The dynamic "+
		"target fraction is used to multiply the (measured) peak flush rate, to compute the flush "+
		"tokens. If the storage.write-stall-nanos indicates significant write stalls, and the granter "+
		"logs show that the dynamic target fraction has already reached the lower bound, one can "+
		"consider lowering it slightly (after consultation with domain experts)", 0.5,
	settings.PositiveFloat)

// DiskBandwidthTokensForElasticEnabled controls whether the disk bandwidth
// resource is considered as a possible bottleneck resource. When it becomes a
// bottleneck, tokens for elastic work are limited based on available disk
// bandwidth. The default is true since actually considering disk bandwidth as
// a bottleneck resource requires additional configuration (outside the
// admission package) to calculate the provisioned bandwidth.
var DiskBandwidthTokensForElasticEnabled = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"admission.disk_bandwidth_tokens.elastic.enabled",
	"when true, and provisioned bandwidth for the disk corresponding to a store is configured, "+
		"tokens for elastic work will be limited if disk bandwidth becomes a bottleneck",
	true,
	settings.WithPublic)

// L0FileCountOverloadThreshold sets a file count threshold that signals an
// overloaded store.
var L0FileCountOverloadThreshold = settings.RegisterIntSetting(
	settings.TenantWritable,
	"admission.l0_file_count_overload_threshold",
	"when the L0 file count exceeds this theshold, the store is considered overloaded",
	l0FileCountOverloadThreshold, settings.PositiveInt)

// L0SubLevelCountOverloadThreshold sets a sub-level count threshold that
// signals an overloaded store.
var L0SubLevelCountOverloadThreshold = settings.RegisterIntSetting(
	settings.TenantWritable,
	"admission.l0_sub_level_count_overload_threshold",
	"when the L0 sub-level count exceeds this threshold, the store is considered overloaded",
	l0SubLevelCountOverloadThreshold, settings.PositiveInt)

// L0MinimumSizePerSubLevel is a minimum size threshold per sub-level, to
// avoid over reliance on the sub-level count as a signal of overload. Pebble
// sometimes has to do frequent flushes of the memtable due to ingesting
// sstables that overlap with the memtable, and each flush may generate a
// sub-level. We have seen situations where these flushes have a tiny amount
// of bytes, but a sequence of these can result in a high sub-level count.
// The default of 5MB is chosen since:
// 5MB*l0SubLevelCountOverloadThreshold=100MB, which can be very quickly
// compacted into Lbase (say Lbase overlapping bytes are 200MB, this is a
// 100MB+200MB=300MB compaction, which takes < 15s).
//
// NB: 5MB is typically significantly smaller than the flush size of a 64MB
// memtable (after accounting for compression when flushing the memtable). If
// it were comparable, this size based computation of sub-levels would
// typically override the actual sub-levels, which would defeat the point of
// using the sub-level count as a metric to guide admission.
//
// Setting this to 0 disables this minimum size logic.
var L0MinimumSizePerSubLevel = settings.RegisterIntSetting(
	settings.SystemOnly,
	"admission.l0_min_size_per_sub_level",
	"when non-zero, this indicates the minimum size that is needed to count towards one sub-level",
	5<<20, settings.NonNegativeInt)

// Experimental observations:
//   - Sub-level count of ~40 caused a node heartbeat latency p90, p99 of 2.5s,
//     4s. With a setting that limits sub-level count to 10, before the system
//     is considered overloaded, and adjustmentInterval = 60, we see the actual
//     sub-level count ranging from 5-30, with p90, p99 node heartbeat latency
//     showing a similar wide range, with 1s, 2s being the middle of the range
//     respectively.
//   - With tpcc, we sometimes see a sub-level count > 10 with only 100 files in
//     L0. We don't want to restrict tokens in this case since the store is able
//     to recover on its own. One possibility would be to require both the
//     thresholds to be exceeded before we consider the store overloaded. But
//     then we run the risk of having 100+ sub-levels when we hit a file count
//     of 1000. Instead we use a sub-level overload threshold of 20.
//
// We've set these overload thresholds in a way that allows the system to
// absorb short durations (say a few minutes) of heavy write load.
const l0FileCountOverloadThreshold = 1000
const l0SubLevelCountOverloadThreshold = 20

// ioLoadListener adjusts tokens in kvStoreTokenGranter for IO, specifically due to
// overload caused by writes. IO uses tokens and not slots since work
// completion is not an indicator that the "resource usage" has ceased -- it
// just means that the write has been applied to the WAL. Most of the work is
// in flushing to sstables and the following compactions, which happens later.
//
// Token units are in bytes and used to protect a number of virtual or
// physical resource bottlenecks:
//   - Compactions out of L0: compactions out of L0 can fall behind and cause
//     too many sub-levels or files in L0.
//   - Flushes into L0: flushes of memtables to L0 can fall behind and cause
//     write stalls due to too many memtables.
//   - Disk bandwidth: there is typically an aggregate read+write provisioned
//     bandwidth, and if it is fully utilized, IO operations can start queueing
//     and encounter high latency.
//
// For simplicity, after ioLoadListener computes the tokens due to compaction
// or flush bottlenecks, it computes the minimum and passes that value to
// granterWithIOTokens.setAvailableIOTokensLocked. That is, instead of working
// with multiple token dimensions, these two token dimensions get collapsed
// into one for enforcement purposes. This also helps simplify the integration
// with WorkQueue which is dealing with a single dimension. The consumption of
// these tokens is based on how many bytes an admitted work adds to L0.
//
// The disk bandwidth constraint is used to compute a token count for elastic
// work (see disk_bandwidth.go for the reasons why this is limited to elastic
// work). Additionally, these tokens are meant be consumed for all incoming
// bytes into the LSM, and not just those written to L0 e.g. ingested bytes
// into L3 should also consume such tokens. Note that we call these disk
// bandwidth tokens, but that is a misnomer -- these are tokens for incoming
// bytes into the LSM, motivated by disk bandwidth as a bottleneck resource,
// and not consumed for every write to the disk (e.g. by compactions). Since
// these tokens are consumed by all incoming bytes into the LSM, and not just
// those into L0, it suggests explicitly modeling this as a separate
// dimension. However, since modeling as a separate dimension everywhere would
// lead to code complexity, we adopt the following compromise:
//
//   - Like the other token dimensions, ioLoadListener computes a different
//     elastic token count (using diskBandwidthLimiter), and a different model
//     for consumption (via
//     storePerWorkTokenEstimator.atDoneDiskBWTokensLinearModel).
//
//   - granterWithIOTokens, implemented by kvStoreTokenGranter, which enforces
//     the token count, also treats this as a separate dimension.
//
//   - WorkQueue works with a single dimension, so the tokens consumed at
//     admission time are based on L0-bytes estimate. However, when
//     StoreWorkQueue informs kvStoreTokenGranter of work completion (by calling
//     storeWriteDone), the tokens are adjusted differently for the
//     flush/compaction L0 tokens and for the "disk bandwidth" tokens.
type ioLoadListener struct {
	storeID     roachpb.StoreID
	settings    *cluster.Settings
	kvRequester storeRequester
	kvGranter   granterWithIOTokens

	// Stats used to compute interval stats.
	statsInitialized bool
	adjustTokensResult
	perWorkTokenEstimator storePerWorkTokenEstimator
	diskBandwidthLimiter  diskBandwidthLimiter

	l0CompactedBytes *metric.Counter
	l0TokensProduced *metric.Counter
}

type ioLoadListenerState struct {
	// Cumulative.
	cumL0AddedBytes uint64
	// Gauge.
	curL0Bytes int64
	// Cumulative.
	cumWriteStallCount      int64
	cumFlushWriteThroughput pebble.ThroughputMetric
	diskBW                  struct {
		// Cumulative
		bytesRead        uint64
		bytesWritten     uint64
		incomingLSMBytes uint64
	}

	// Exponentially smoothed per interval values.

	smoothedIntL0CompactedBytes int64 // bytes leaving L0
	// Smoothed history of byte tokens calculated based on compactions out of L0.
	smoothedCompactionByteTokens float64

	// Smoothed history of flush tokens calculated based on memtable flushes,
	// before multiplying by target fraction.
	smoothedNumFlushTokens float64
	// The target fraction to be used for the effective flush tokens. It is in
	// the interval
	// [MinFlushUtilizationFraction,maxFlushUtilTargetFraction].
	flushUtilTargetFraction float64

	// totalNumByteTokens represents the tokens to give out until the next call to
	// adjustTokens. They are parceled out in small intervals. byteTokensAllocated
	// represents what has been given out.
	totalNumByteTokens  int64
	byteTokensAllocated int64
	// Used tokens can be negative if some tokens taken in one interval were
	// returned in another, but that will be extremely rare.
	byteTokensUsed              int64
	byteTokensUsedByElasticWork int64

	totalNumElasticByteTokens  int64
	elasticByteTokensAllocated int64

	// elasticDiskBWTokens represents the tokens to give out until the next call
	// to adjustTokens. They are parceled out in small intervals.
	// elasticDiskTokensAllocated represents what has been given out.
	elasticDiskBWTokens          int64
	elasticDiskBWTokensAllocated int64
}

const unlimitedTokens = math.MaxInt64

// Token changes are made at a coarse time granularity of 15s since
// compactions can take ~10s to complete. The totalNumByteTokens to give out over
// the 15s interval are given out in a smoothed manner, at either 1ms intervals,
// or 250ms intervals depending on system load.
// This has similarities with the following kinds of token buckets:
//   - Zero replenishment rate and a burst value that is changed every 15s. We
//     explicitly don't want a huge burst every 15s.
//   - For loaded systems, a replenishment rate equal to
//     totalNumByteTokens/15000(once per ms), with a burst capped at
//     totalNumByteTokens/60.
//   - For unloaded systems, a replenishment rate equal to
//     totalNumByteTokens/60(once per 250ms), with a burst capped at
//     totalNumByteTokens/60.
//   - The only difference with the code here is that if totalNumByteTokens is
//     small, the integer rounding effects are compensated for.
//
// In an experiment with extreme overload using KV0 with block size 64KB,
// and 4096 clients, we observed the following states of L0 at 1min
// intervals (r-amp is the L0 sub-level count), in the absence of any
// admission control:
//
// __level_____count____size___score______in__ingest(sz_cnt)____move(sz_cnt)___write(sz_cnt)____read___r-amp___w-amp›
//
//	0        96   158 M    2.09   315 M     0 B       0     0 B       0   305 M     178     0 B       3     1.0›
//	0      1026   1.7 G    3.15   4.7 G     0 B       0     0 B       0   4.7 G   2.8 K     0 B      24     1.0›
//	0      1865   3.0 G    2.86   9.1 G     0 B       0     0 B       0   9.1 G   5.5 K     0 B      38     1.0›
//	0      3225   4.9 G    3.46    13 G     0 B       0     0 B       0    13 G   8.3 K     0 B      59     1.0›
//	0      4720   7.0 G    3.46    17 G     0 B       0     0 B       0    17 G    11 K     0 B      85     1.0›
//	0      6120   9.0 G    4.13    21 G     0 B       0     0 B       0    21 G    14 K     0 B     109     1.0›
//
// Note the fast growth in sub-level count. Production issues typically have
// slower growth towards an unhealthy state (remember that similar stats in
// the logs of a regular CockroachDB node are at 10min intervals, and not at
// 1min).
//
// In the above experiment, L0 compaction durations at 200+ sub-levels were
// usually sane, with most L0 compactions < 10s, and with a bandwidth of
// ~80MB/s. There were some 1-2GB compactions that took ~20s. The
// expectation is that with the throttling done by admission control here,
// we should almost never see multi-minute compactions. Which makes it
// reasonable to simply use metrics that are updated when compactions
// complete (as opposed to also tracking progress in bytes of on-going
// compactions).
//
// An interval < 250ms is picked to hand out the computed tokens due to the needs
// of flush tokens. For compaction tokens, a 1s interval is fine-grained enough.
//
// If flushing a memtable takes 100ms, then 10 memtables can be sustainably
// flushed in 1s. If we dole out flush tokens in 1s intervals, then there are
// enough tokens to create 10 memtables at the very start of a 1s interval,
// which will cause a write stall. Intuitively, the faster it is to flush a
// memtable, the smaller the interval for doling out these tokens. We have
// observed flushes taking ~0.5s, so we need to pick an interval less than 0.5s,
// say 250ms, for doling out these tokens.
//
// We use a 1ms interval for handing out tokens, to avoid upto 250ms wait times
// for high priority requests. As a simple example, consider a scenario where
// each request needs 1 byte token, and there are 1000 tokens added every 250ms.
// There is a uniform arrival rate of 2000 high priority requests/s, so 500
// requests uniformly distributed over 250ms. And a uniform arrival rate of
// 10,000/s of low priority requests, so 2500 requests uniformly distributed over
// 250ms. There are more than enough tokens to fully satisfy the high priority
// tokens (they use only 50% of the tokens), but not enough for the low priority
// requests. Ignore the fact that the latter will result in indefinite queue
// growth in the admission control WorkQueue. At a particular 250ms tick, the
// token bucket will go from 0 tokens to 1000 tokens. Any queued high priority
// requests will be immediately granted their token, until there are no queued
// high priority requests. Then since there are always a large number of low
// priority requests waiting, they will be granted until 0 tokens remain. Now we
// have a 250ms duration until the next replenishment and 0 tokens, so any high
// priority requests arriving will have to wait. The maximum wait time is 250ms.
//
// We use a 250ms intervals for underloaded systems, to avoid CPU utilization
// issues (see the discussion in runnable.go).
const adjustmentInterval = 15

type tickDuration time.Duration

func (t tickDuration) ticksInAdjustmentInterval() int64 {
	return 15 * int64(time.Second/time.Duration(t))
}

const unloadedDuration = tickDuration(250 * time.Millisecond)
const loadedDuration = tickDuration(1 * time.Millisecond)

// tokenAllocationTicker wraps a time.Ticker, and also computes the remaining
// ticks in the adjustment interval, given an expected tick rate. If every tick
// from the ticker was always equal to the expected tick rate, then we could
// easily determine the remaining ticks, but each tick of time.Ticker can have
// drift, especially for tiny tick rates like 1ms.
type tokenAllocationTicker struct {
	expectedTickDuration        time.Duration
	adjustmentIntervalStartTime time.Time
	ticker                      *time.Ticker
}

// Start a new adjustment interval. adjustmentStart must be called before tick
// is called. After the initial call, adjustmentStart must also be called if
// remainingticks returns 0, to indicate that a new adjustment interval has
// started.
func (t *tokenAllocationTicker) adjustmentStart(loaded bool) {
	// For each adjustmentInterval, we pick a tick rate depending on the system
	// load. If the system is unloaded, we tick at a 250ms rate, and if the system
	// is loaded, we tick at a 1ms rate. See the comment above the
	// adjustmentInterval definition to see why we tick at different rates.
	tickDuration := unloadedDuration
	if loaded {
		tickDuration = loadedDuration
	}
	t.expectedTickDuration = time.Duration(tickDuration)
	if t.ticker == nil {
		t.ticker = time.NewTicker(t.expectedTickDuration)
	} else {
		t.ticker.Reset(t.expectedTickDuration)
	}
	t.adjustmentIntervalStartTime = timeutil.Now()
}

func (t *tokenAllocationTicker) tick() {
	<-t.ticker.C
}

// remainingTicks will return the remaining ticks before the next adjustment
// interval is reached while assuming that all future ticks will have a duration of
// expectedTickDuration. A return value of 0 indicates that adjustmentStart must
// be called, as the previous adjustmentInterval is over.
func (t *tokenAllocationTicker) remainingTicks() uint64 {
	timePassed := timeutil.Since(t.adjustmentIntervalStartTime)
	if timePassed > adjustmentInterval*time.Second {
		return 0
	}
	remainingTime := adjustmentInterval*time.Second - timePassed
	return uint64((remainingTime + t.expectedTickDuration - 1) / t.expectedTickDuration)
}

func (t *tokenAllocationTicker) stop() {
	t.ticker.Stop()
	*t = tokenAllocationTicker{}
}

func cumLSMWriteAndIngestedBytes(
	m *pebble.Metrics,
) (writeAndIngestedBytes uint64, ingestedBytes uint64) {
	for i := range m.Levels {
		writeAndIngestedBytes += m.Levels[i].BytesIngested + m.Levels[i].BytesFlushed
		ingestedBytes += m.Levels[i].BytesIngested
	}
	return writeAndIngestedBytes, ingestedBytes
}

// pebbleMetricsTicks is called every adjustmentInterval seconds, and decides
// the token allocations until the next call. Returns true iff the system is
// loaded.
func (io *ioLoadListener) pebbleMetricsTick(ctx context.Context, metrics StoreMetrics) bool {
	ctx = logtags.AddTag(ctx, "s", io.storeID)
	m := metrics.Metrics
	if !io.statsInitialized {
		io.statsInitialized = true
		sas := io.kvRequester.getStoreAdmissionStats()
		cumLSMIncomingBytes, cumLSMIngestedBytes := cumLSMWriteAndIngestedBytes(metrics.Metrics)
		io.perWorkTokenEstimator.updateEstimates(metrics.Levels[0], cumLSMIngestedBytes, sas)
		io.adjustTokensResult = adjustTokensResult{
			ioLoadListenerState: ioLoadListenerState{
				cumL0AddedBytes:    m.Levels[0].BytesFlushed + m.Levels[0].BytesIngested,
				curL0Bytes:         m.Levels[0].Size,
				cumWriteStallCount: metrics.WriteStallCount,
				// No initial limit, i.e, the first interval is unlimited.
				totalNumByteTokens:        unlimitedTokens,
				totalNumElasticByteTokens: unlimitedTokens,
				elasticDiskBWTokens:       unlimitedTokens,
			},
			aux: adjustTokensAuxComputations{},
			ioThreshold: &admissionpb.IOThreshold{
				L0NumSubLevels:           int64(m.Levels[0].Sublevels),
				L0NumSubLevelsThreshold:  math.MaxInt64,
				L0NumFiles:               m.Levels[0].NumFiles,
				L0NumFilesThreshold:      math.MaxInt64,
				L0Size:                   m.Levels[0].Size,
				L0MinimumSizePerSubLevel: 0,
			},
		}
		io.diskBW.bytesRead = metrics.DiskStats.BytesRead
		io.diskBW.bytesWritten = metrics.DiskStats.BytesWritten
		io.diskBW.incomingLSMBytes = cumLSMIncomingBytes
		io.cumFlushWriteThroughput = metrics.Flush.WriteThroughput
		io.copyAuxEtcFromPerWorkEstimator()

		// Assume system starts off unloaded.
		return false
	}
	io.adjustTokens(ctx, metrics)
	io.cumFlushWriteThroughput = metrics.Flush.WriteThroughput
	// We assume that the system is loaded if there is less than unlimited tokens
	// available.
	return io.totalNumByteTokens < unlimitedTokens || io.totalNumElasticByteTokens < unlimitedTokens
}

// For both byte and disk bandwidth tokens, allocateTokensTick gives out
// remainingTokens/remainingTicks tokens in the current tick.
func (io *ioLoadListener) allocateTokensTick(remainingTicks int64) {
	allocateFunc := func(total int64, allocated int64, remainingTicks int64) (toAllocate int64) {
		remainingTokens := total - allocated
		// remainingTokens can be equal to unlimitedTokens(MaxInt64) if allocated ==
		// 0. In such cases remainingTokens + remainingTicks - 1 will overflow.
		if remainingTokens >= unlimitedTokens-(remainingTicks-1) {
			toAllocate = remainingTokens / remainingTicks
		} else {
			// Round up so that we don't accumulate tokens to give in a burst on
			// the last tick.
			//
			// TODO(bananabrick): Rounding up is a problem for 1ms tick rate as we tick
			// up to 15000 times. Say totalNumByteTokens is 150001. We round up to give
			// 11 tokens per ms. So, we'll end up distributing the 150001 available
			// tokens in 150000/11 == 13637 remainingTicks. So, we'll have over a
			// second where we grant no tokens. Larger values of totalNumBytesTokens
			// will ease this problem.
			toAllocate = (remainingTokens + remainingTicks - 1) / remainingTicks
			if toAllocate < 0 {
				panic(errors.AssertionFailedf("toAllocate is negative %d", toAllocate))
			}
			if toAllocate+allocated > total {
				toAllocate = total - allocated
			}
		}
		return toAllocate
	}
	// INVARIANT: toAllocate* >= 0.
	toAllocateByteTokens := allocateFunc(
		io.totalNumByteTokens,
		io.byteTokensAllocated,
		remainingTicks,
	)
	if toAllocateByteTokens < 0 {
		panic(errors.AssertionFailedf("toAllocateByteTokens is negative %d", toAllocateByteTokens))
	}
	toAllocateElasticByteTokens := allocateFunc(
		io.totalNumElasticByteTokens, io.elasticByteTokensAllocated, remainingTicks)
	if toAllocateElasticByteTokens < 0 {
		panic(errors.AssertionFailedf("toAllocateElasticByteTokens is negative %d",
			toAllocateElasticByteTokens))
	}
	toAllocateElasticDiskBWTokens :=
		allocateFunc(
			io.elasticDiskBWTokens,
			io.elasticDiskBWTokensAllocated,
			remainingTicks,
		)
	if toAllocateElasticDiskBWTokens < 0 {
		panic(errors.AssertionFailedf("toAllocateElasticDiskBWTokens is negative %d",
			toAllocateElasticDiskBWTokens))
	}
	// INVARIANT: toAllocate >= 0.
	io.byteTokensAllocated += toAllocateByteTokens
	if io.byteTokensAllocated < 0 {
		panic(errors.AssertionFailedf("tokens allocated is negative %d", io.byteTokensAllocated))
	}
	io.elasticByteTokensAllocated += toAllocateElasticByteTokens
	if io.elasticByteTokensAllocated < 0 {
		panic(errors.AssertionFailedf(
			"tokens allocated is negative %d", io.elasticByteTokensAllocated))
	}
	io.elasticDiskBWTokensAllocated += toAllocateElasticDiskBWTokens

	tokensMaxCapacity := allocateFunc(
		io.totalNumByteTokens, 0, unloadedDuration.ticksInAdjustmentInterval(),
	)
	elasticTokensMaxCapacity := allocateFunc(
		io.totalNumElasticByteTokens, 0, unloadedDuration.ticksInAdjustmentInterval())
	diskBWTokenMaxCapacity := allocateFunc(
		io.elasticDiskBWTokens, 0, unloadedDuration.ticksInAdjustmentInterval(),
	)
	tokensUsed, tokensUsedByElasticWork := io.kvGranter.setAvailableTokens(
		toAllocateByteTokens,
		toAllocateElasticByteTokens,
		toAllocateElasticDiskBWTokens,
		tokensMaxCapacity,
		elasticTokensMaxCapacity,
		diskBWTokenMaxCapacity,
		remainingTicks == 1,
	)
	io.byteTokensUsed += tokensUsed
	io.byteTokensUsedByElasticWork += tokensUsedByElasticWork
}

func computeIntervalDiskLoadInfo(
	prevCumBytesRead uint64, prevCumBytesWritten uint64, diskStats DiskStats,
) intervalDiskLoadInfo {
	return intervalDiskLoadInfo{
		readBandwidth:        int64((diskStats.BytesRead - prevCumBytesRead) / adjustmentInterval),
		writeBandwidth:       int64((diskStats.BytesWritten - prevCumBytesWritten) / adjustmentInterval),
		provisionedBandwidth: diskStats.ProvisionedBandwidth,
	}
}

// adjustTokens computes a new value of totalNumByteTokens (and resets
// tokensAllocated). The new value, when overloaded, is based on comparing how
// many bytes are being moved out of L0 via compactions with the average
// number of bytes being added to L0 per KV work. We want the former to be
// (significantly) larger so that L0 returns to a healthy state. The byte
// token computation also takes into account the flush throughput, since an
// inability to flush fast enough can result in write stalls due to high
// memtable counts, which we want to avoid as it can cause latency hiccups of
// 100+ms for all write traffic.
func (io *ioLoadListener) adjustTokens(ctx context.Context, metrics StoreMetrics) {
	sas := io.kvRequester.getStoreAdmissionStats()
	// Copy the cumulative disk bandwidth values for later use.
	cumDiskBW := io.ioLoadListenerState.diskBW
	wt := metrics.Flush.WriteThroughput
	wt.Subtract(io.cumFlushWriteThroughput)

	res := io.adjustTokensInner(ctx, io.ioLoadListenerState,
		metrics.Levels[0], metrics.WriteStallCount, wt,
		L0FileCountOverloadThreshold.Get(&io.settings.SV),
		L0SubLevelCountOverloadThreshold.Get(&io.settings.SV),
		L0MinimumSizePerSubLevel.Get(&io.settings.SV),
		MinFlushUtilizationFraction.Get(&io.settings.SV),
	)
	io.adjustTokensResult = res
	cumLSMIncomingBytes, cumLSMIngestedBytes := cumLSMWriteAndIngestedBytes(metrics.Metrics)
	{
		// Disk Bandwidth tokens.
		io.aux.diskBW.intervalDiskLoadInfo = computeIntervalDiskLoadInfo(
			cumDiskBW.bytesRead, cumDiskBW.bytesWritten, metrics.DiskStats)
		diskTokensUsed := io.kvGranter.getDiskTokensUsedAndReset()
		io.aux.diskBW.intervalLSMInfo = intervalLSMInfo{
			incomingBytes:     int64(cumLSMIncomingBytes) - int64(cumDiskBW.incomingLSMBytes),
			regularTokensUsed: diskTokensUsed[admissionpb.RegularWorkClass],
			elasticTokensUsed: diskTokensUsed[admissionpb.ElasticWorkClass],
		}
		if metrics.DiskStats.ProvisionedBandwidth > 0 {
			io.elasticDiskBWTokens = io.diskBandwidthLimiter.computeElasticTokens(ctx,
				io.aux.diskBW.intervalDiskLoadInfo, io.aux.diskBW.intervalLSMInfo)
			io.elasticDiskBWTokensAllocated = 0
		}
		if metrics.DiskStats.ProvisionedBandwidth == 0 ||
			!DiskBandwidthTokensForElasticEnabled.Get(&io.settings.SV) {
			io.elasticDiskBWTokens = unlimitedTokens
		}
		io.diskBW.bytesRead = metrics.DiskStats.BytesRead
		io.diskBW.bytesWritten = metrics.DiskStats.BytesWritten
		io.diskBW.incomingLSMBytes = cumLSMIncomingBytes
	}
	io.perWorkTokenEstimator.updateEstimates(metrics.Levels[0], cumLSMIngestedBytes, sas)
	io.copyAuxEtcFromPerWorkEstimator()
	requestEstimates := io.perWorkTokenEstimator.getStoreRequestEstimatesAtAdmission()
	io.kvRequester.setStoreRequestEstimates(requestEstimates)
	l0WriteLM, l0IngestLM, ingestLM := io.perWorkTokenEstimator.getModelsAtDone()
	io.kvGranter.setLinearModels(l0WriteLM, l0IngestLM, ingestLM)
	if io.aux.doLogFlush || io.elasticDiskBWTokens != unlimitedTokens || log.V(1) {
		log.Infof(ctx, "IO overload: %s", io.adjustTokensResult)
	}
}

// copyAuxEtcFromPerWorkEstimator copies the auxiliary and other numerical
// state from io.perWorkTokenEstimator. This is helpful in keeping all the
// numerical state for understanding the behavior of ioLoadListener and its
// helpers in one place for simplicity of logging.
func (io *ioLoadListener) copyAuxEtcFromPerWorkEstimator() {
	// Copy the aux so that the printing story is simplified.
	io.adjustTokensResult.aux.perWorkTokensAux = io.perWorkTokenEstimator.aux
	requestEstimates := io.perWorkTokenEstimator.getStoreRequestEstimatesAtAdmission()
	io.adjustTokensResult.requestEstimates = requestEstimates
	l0WriteLM, l0IngestLM, ingestLM := io.perWorkTokenEstimator.getModelsAtDone()
	io.adjustTokensResult.l0WriteLM = l0WriteLM
	io.adjustTokensResult.l0IngestLM = l0IngestLM
	io.adjustTokensResult.ingestLM = ingestLM
}

type tokenKind int8

const (
	compactionTokenKind tokenKind = iota
	flushTokenKind
)

// adjustTokensAuxComputations encapsulates auxiliary numerical state for
// ioLoadListener that is helpful for understanding its behavior.
type adjustTokensAuxComputations struct {
	intL0AddedBytes     int64
	intL0CompactedBytes int64

	intFlushTokens      float64
	intFlushUtilization float64
	intWriteStalls      int64

	prevTokensUsed              int64
	prevTokensUsedByElasticWork int64
	tokenKind                   tokenKind

	perWorkTokensAux perWorkTokensAux
	doLogFlush       bool

	diskBW struct {
		intervalDiskLoadInfo intervalDiskLoadInfo
		intervalLSMInfo      intervalLSMInfo
	}
}

// adjustTokensInner is used for computing tokens based on compaction and
// flush bottlenecks.
func (io *ioLoadListener) adjustTokensInner(
	ctx context.Context,
	prev ioLoadListenerState,
	l0Metrics pebble.LevelMetrics,
	cumWriteStallCount int64,
	flushWriteThroughput pebble.ThroughputMetric,
	threshNumFiles, threshNumSublevels int64,
	l0MinSizePerSubLevel int64,
	minFlushUtilTargetFraction float64,
) adjustTokensResult {
	ioThreshold := &admissionpb.IOThreshold{
		L0NumFiles:               l0Metrics.NumFiles,
		L0NumFilesThreshold:      threshNumFiles,
		L0NumSubLevels:           int64(l0Metrics.Sublevels),
		L0NumSubLevelsThreshold:  threshNumSublevels,
		L0Size:                   l0Metrics.Size,
		L0MinimumSizePerSubLevel: l0MinSizePerSubLevel,
	}

	curL0Bytes := l0Metrics.Size
	cumL0AddedBytes := l0Metrics.BytesFlushed + l0Metrics.BytesIngested
	// L0 growth over the last interval.
	intL0AddedBytes := int64(cumL0AddedBytes) - int64(prev.cumL0AddedBytes)
	if intL0AddedBytes < 0 {
		// intL0AddedBytes is a simple delta computation over individually cumulative
		// stats, so should not be negative.
		log.Warningf(ctx, "intL0AddedBytes %d is negative", intL0AddedBytes)
		intL0AddedBytes = 0
	}
	// intL0CompactedBytes are due to finished compactions.
	intL0CompactedBytes := prev.curL0Bytes + intL0AddedBytes - curL0Bytes
	if intL0CompactedBytes < 0 {
		// Ignore potential inconsistencies across cumulative stats and current L0
		// bytes (gauge).
		intL0CompactedBytes = 0
	}
	io.l0CompactedBytes.Inc(intL0CompactedBytes)

	const alpha = 0.5

	// Compaction scheduling can be uneven in prioritizing L0 for compactions,
	// so smooth out what is being removed by compactions.
	smoothedIntL0CompactedBytes := int64(alpha*float64(intL0CompactedBytes) + (1-alpha)*float64(prev.smoothedIntL0CompactedBytes))

	// Flush tokens:
	//
	// Write stalls happen when flushing of memtables is a bottleneck.
	//
	// Computing Flush Tokens:
	// Flush can go from not being the bottleneck in one 15s interval
	// (adjustmentInterval) to being the bottleneck in the next 15s interval
	// (e.g. when L0 falls below the unhealthy threshold and compaction tokens
	// become unlimited). So the flush token limit has to react quickly (cannot
	// afford to wait for multiple 15s intervals). We've observed that if we
	// normalize the flush rate based on flush loop utilization (the PeakRate
	// computation below), and use that to compute flush tokens, the token
	// counts are quite stable. Here are two examples, showing this steady token
	// count computed using PeakRate of the flush ThroughputMetric, despite
	// changes in flush loop utilization (the util number below).
	//
	// Example 1: Case where IO bandwidth was not a bottleneck
	// flush: tokens: 2312382401, util: 0.90
	// flush: tokens: 2345477107, util: 0.31
	// flush: tokens: 2317829891, util: 0.29
	// flush: tokens: 2428387843, util: 0.17
	//
	// Example 2: Case where IO bandwidth became a bottleneck (and mean fsync
	// latency was fluctuating between 1ms and 4ms in the low util to high util
	// cases).
	//
	// flush: tokens: 1406132615, util: 1.00
	// flush: tokens: 1356476227, util: 0.64
	// flush: tokens: 1374880806, util: 0.24
	// flush: tokens: 1328578534, util: 0.96
	//
	// Hence, using PeakRate as a basis for computing flush tokens seems sound.
	// The other important question is what fraction of PeakRate avoids write
	// stalls. It is likely less than 100% since while a flush is ongoing,
	// memtables can accumulate and cause a stall. For example, we have observed
	// write stalls at 80% of PeakRate. The fraction depends on configuration
	// parameters like MemTableStopWritesThreshold (defaults to 4 in
	// CockroachDB), and environmental and workload factors like how long a
	// flush takes to flush a single 64MB memtable. Instead of trying to measure
	// and adjust for these, we use a simple multiplier,
	// flushUtilTargetFraction. By default, flushUtilTargetFraction ranges
	// between 0.5 and 1.5. The lower bound is configurable via
	// admission.min_flush_util_percent and if configured above the upper bound,
	// the upper bound will be ignored and the target fraction will not be
	// dynamically adjusted. The dynamic adjustment logic uses an additive step
	// size of flushUtilTargetFractionIncrement (0.025), with the following
	// logic:
	// - Reduce the fraction if there is a write-stall. The reduction may use a
	//   small multiple of flushUtilTargetFractionIncrement. This is so that
	//   this probing spends more time below the threshold where write stalls
	//   occur.
	// - Increase fraction if no write-stall and flush tokens were almost all
	//   used.
	//
	// This probing unfortunately cannot eliminate write stalls altogether.
	// Future improvements could use more history to settle on a good
	// flushUtilTargetFraction for longer, or use some measure of how close we
	// are to a write-stall to stop the increase.
	//
	// Ingestion and flush tokens:
	//
	// Ingested sstables do not utilize any flush capacity. Consider 2 cases:
	// - sstable ingested into L0: there was either data overlap with L0, or
	//   file boundary overlap with L0-L6. To be conservative, lets assume there
	//   was data overlap, and that this data overlap extended into the memtable
	//   at the time of ingestion. Memtable(s) would have been force flushed to
	//   handle such overlap. The cost of flushing a memtable is based on how
	//   much of the allocated memtable capacity is used, so an early flush
	//   seems harmless. However, write stalls are based on allocated memtable
	//   capacity, so there is a potential negative interaction of these forced
	//   flushes since they cause additional memtable capacity allocation.
	// - sstable ingested into L1-L6: there was no data overlap with L0, which
	//   implies that there was no reason to flush memtables.
	//
	// Since there is some interaction noted in bullet 1, and because it
	// simplifies the admission control token behavior, we use flush tokens in
	// an identical manner as compaction tokens -- to be consumed by all data
	// flowing into L0. Some of this conservative choice will be compensated for
	// by flushUtilTargetFraction (when the mix of ingestion and actual flushes
	// are stable). Another thing to note is that compactions out of L0 are
	// typically the more persistent bottleneck than flushes for the following
	// reason:
	// There is a dedicated flush thread. With a maximum compaction concurrency
	// of C, we have up to C threads dedicated to handling the write-amp of W
	// (caused by rewriting the same data). So C/(W-1) threads on average are
	// reading the original data (that will be rewritten W-1 times). Since L0
	// can have multiple overlapping files, and intra-L0 compactions are usually
	// avoided, we can assume (at best) that the original data (in L0) is being
	// read only when compacting to levels lower than L0. That is, C/(W-1)
	// threads are reading from L0 to compact to levels lower than L0. Since W
	// can be 20+ and C defaults to 3 (we plan to dynamically adjust C but one
	// can expect C to be <= 10), C/(W-1) < 1. So the main reason we are
	// considering flush tokens is transient flush bottlenecks, and workloads
	// where W is small.

	// Compute flush utilization for this interval. A very low flush utilization
	// will cause flush tokens to be unlimited.
	intFlushUtilization := float64(0)
	if flushWriteThroughput.WorkDuration > 0 {
		intFlushUtilization = float64(flushWriteThroughput.WorkDuration) /
			float64(flushWriteThroughput.WorkDuration+flushWriteThroughput.IdleDuration)
	}
	// Compute flush tokens for this interval that would cause 100% utilization.
	intFlushTokens := float64(flushWriteThroughput.PeakRate()) * adjustmentInterval
	intWriteStalls := cumWriteStallCount - prev.cumWriteStallCount

	// Ensure flushUtilTargetFraction is in the configured bounds. This also
	// does lazy initialization.
	const maxFlushUtilTargetFraction = 1.5
	flushUtilTargetFraction := prev.flushUtilTargetFraction
	if flushUtilTargetFraction == 0 {
		// Initialization: use the maximum configured fraction.
		flushUtilTargetFraction = minFlushUtilTargetFraction
		if flushUtilTargetFraction < maxFlushUtilTargetFraction {
			flushUtilTargetFraction = maxFlushUtilTargetFraction
		}
	} else if flushUtilTargetFraction < minFlushUtilTargetFraction {
		// The min can be changed in a running system, so we bump up to conform to
		// the min.
		flushUtilTargetFraction = minFlushUtilTargetFraction
	}
	numFlushTokens := int64(unlimitedTokens)
	// doLogFlush becomes true if something interesting is done here.
	doLogFlush := false
	smoothedNumFlushTokens := prev.smoothedNumFlushTokens
	const flushUtilIgnoreThreshold = 0.05
	if intFlushUtilization > flushUtilIgnoreThreshold {
		if smoothedNumFlushTokens == 0 {
			// Initialization.
			smoothedNumFlushTokens = intFlushTokens
		} else {
			smoothedNumFlushTokens = alpha*intFlushTokens + (1-alpha)*prev.smoothedNumFlushTokens
		}
		const flushUtilTargetFractionIncrement = 0.025
		// Have we used, over the last (15s) cycle, more than 90% of the tokens we
		// would give out for the next cycle? If yes, highTokenUsage is true.
		highTokenUsage :=
			float64(prev.byteTokensUsed) >= 0.9*smoothedNumFlushTokens*flushUtilTargetFraction
		if intWriteStalls > 0 {
			// Try decrease since there were write-stalls.
			numDecreaseSteps := 1
			// These constants of 5, 3, 2, 2 were found to work reasonably well,
			// without causing large decreases. We need better benchmarking to tune
			// such constants.
			if intWriteStalls >= 5 {
				numDecreaseSteps = 3
			} else if intWriteStalls >= 2 {
				numDecreaseSteps = 2
			}
			for i := 0; i < numDecreaseSteps; i++ {
				if flushUtilTargetFraction >= minFlushUtilTargetFraction+flushUtilTargetFractionIncrement {
					flushUtilTargetFraction -= flushUtilTargetFractionIncrement
					doLogFlush = true
				} else {
					break
				}
			}
		} else if flushUtilTargetFraction < maxFlushUtilTargetFraction-flushUtilTargetFractionIncrement &&
			intWriteStalls == 0 && highTokenUsage {
			// No write-stalls, and token usage was high, so give out more tokens.
			flushUtilTargetFraction += flushUtilTargetFractionIncrement
			doLogFlush = true
		}
		if highTokenUsage {
			doLogFlush = true
		}
		flushTokensFloat := flushUtilTargetFraction * smoothedNumFlushTokens
		if flushTokensFloat < float64(math.MaxInt64) {
			numFlushTokens = int64(flushTokensFloat)
		}
		// Else avoid overflow by using the previously set unlimitedTokens. This
		// should not really happen.
	}
	// Else intFlushUtilization is too low. We don't want to make token
	// determination based on a very low utilization, so we hand out unlimited
	// tokens. Note that flush utilization has been observed to fluctuate from
	// 0.16 to 0.9 in a single interval, when compaction tokens are not limited,
	// hence we have set flushUtilIgnoreThreshold to a very low value. If we've
	// erred towards it being too low, we run the risk of computing incorrect
	// tokens. If we've erred towards being too high, we run the risk of giving
	// out unlimitedTokens and causing write stalls.

	// We constrain admission based on compactions, if the store is over the L0
	// threshold.
	var totalNumByteTokens int64
	var smoothedCompactionByteTokens float64

	score, _ := ioThreshold.Score()
	// Multiplying score by 2 for ease of calculation.
	score *= 2
	// We define four levels of load:
	// Let C be smoothedIntL0CompactedBytes.
	//
	// Underload: Score is less than 0.5, which means sublevels is less than 5.
	// In this case, we don't limit compaction tokens. Flush tokens will likely
	// become the limit.
	//
	// Low load: Score is >= 0.5 and score is less than 1. In this case, we limit
	// compaction tokens, and interpolate between C tokens when score is 1, and
	// 2C tokens when score is 0.5.
	//
	// Medium load: Score is >= 1 and < 2. We limit compaction tokens, and limit
	// them between C and C/2 tokens when score is 1 and 2 respectively.
	//
	// Overload: Score is >= 2. We limit compaction tokens, and limit tokens to
	// at most C/2 tokens.
	if score < 0.5 {
		// Underload. Maintain a smoothedCompactionByteTokens based on what was
		// removed, so that when we go over the threshold we have some history.
		// This is also useful when we temporarily dip below the threshold --
		// we've seen extreme situations with alternating 15s intervals of above
		// and below the threshold.
		numTokens := intL0CompactedBytes
		// Smooth it. This may seem peculiar since we are already using
		// smoothedIntL0CompactedBytes, but the clauses below use different
		// computations so we also want the history of smoothedCompactionByteTokens.
		smoothedCompactionByteTokens = alpha*float64(numTokens) + (1-alpha)*prev.smoothedCompactionByteTokens
		totalNumByteTokens = unlimitedTokens
	} else {
		doLogFlush = true
		var fTotalNumByteTokens float64
		if score >= 2 {
			// Overload.
			//
			// Don't admit more byte work than we can remove via compactions.
			// totalNumByteTokens tracks our goal for admission. Scale down
			// since we want to get under the thresholds over time.
			fTotalNumByteTokens = float64(smoothedIntL0CompactedBytes / 2.0)
		} else if score >= 0.5 && score < 1 {
			// Low load. Score in [0.5, 1). Tokens should be
			// smoothedIntL0CompactedBytes at 1, and 2 * smoothedIntL0CompactedBytes
			// at 0.5.
			fTotalNumByteTokens = -score*(2*float64(smoothedIntL0CompactedBytes)) + 3*float64(smoothedIntL0CompactedBytes)
		} else {
			// Medium load. Score in [1, 2). We use linear interpolation from
			// medium load to overload, to slowly give out fewer tokens as we
			// move towards overload.
			halfSmoothedBytes := float64(smoothedIntL0CompactedBytes / 2.0)
			fTotalNumByteTokens = -score*halfSmoothedBytes + 3*halfSmoothedBytes
		}
		smoothedCompactionByteTokens = alpha*fTotalNumByteTokens + (1-alpha)*prev.smoothedCompactionByteTokens
		if float64(math.MaxInt64) < smoothedCompactionByteTokens {
			// Avoid overflow. This should not really happen.
			totalNumByteTokens = math.MaxInt64
		} else {
			totalNumByteTokens = int64(smoothedCompactionByteTokens)
		}
	}

	totalNumElasticByteTokens := int64(unlimitedTokens)
	// NB: score == (num-sublevels / 20) * 2 = num-sublevels/10 (we are ignoring
	// the rare case where score is determined by file count). So score >= 0.1
	// means that we start shaping when there is 1 sublevel. sublevel / 10 >=
	if score >= 0.1 {
		doLogFlush = true
		// Use a linear function with slope of -1.25 and compaction tokens of
		// 1.25*compaction-bandwidth at score of 0.1. At a score of 0.5 (5
		// sublevels) the tokens will be 0.75*compaction-bandwidth. Experimental
		// results show the sublevels hovering around 3, as expected.
		//
		// NB: at score >= 1.1 (11 sublevels), there are 0 elastic tokens.
		totalNumElasticByteTokens = int64(float64(smoothedIntL0CompactedBytes) *
			(1.25 - 1.25*(score-0.1)))

		totalNumElasticByteTokens = max(totalNumElasticByteTokens, 1)
	}
	// Use the minimum of the token count calculated using compactions and
	// flushes.
	tokenKind := compactionTokenKind
	if totalNumByteTokens > numFlushTokens {
		totalNumByteTokens = numFlushTokens
		tokenKind = flushTokenKind
	}
	if totalNumElasticByteTokens > totalNumByteTokens {
		totalNumElasticByteTokens = totalNumByteTokens
	}

	io.l0TokensProduced.Inc(totalNumByteTokens)

	// Install the latest cumulative stats.
	return adjustTokensResult{
		ioLoadListenerState: ioLoadListenerState{
			cumL0AddedBytes:              cumL0AddedBytes,
			curL0Bytes:                   curL0Bytes,
			cumWriteStallCount:           cumWriteStallCount,
			smoothedIntL0CompactedBytes:  smoothedIntL0CompactedBytes,
			smoothedCompactionByteTokens: smoothedCompactionByteTokens,
			smoothedNumFlushTokens:       smoothedNumFlushTokens,
			flushUtilTargetFraction:      flushUtilTargetFraction,
			totalNumByteTokens:           totalNumByteTokens,
			byteTokensAllocated:          0,
			byteTokensUsed:               0,
			byteTokensUsedByElasticWork:  0,
			totalNumElasticByteTokens:    totalNumElasticByteTokens,
			elasticByteTokensAllocated:   0,
		},
		aux: adjustTokensAuxComputations{
			intL0AddedBytes:             intL0AddedBytes,
			intL0CompactedBytes:         intL0CompactedBytes,
			intFlushTokens:              intFlushTokens,
			intFlushUtilization:         intFlushUtilization,
			intWriteStalls:              intWriteStalls,
			prevTokensUsed:              prev.byteTokensUsed,
			prevTokensUsedByElasticWork: prev.byteTokensUsedByElasticWork,
			tokenKind:                   tokenKind,
			doLogFlush:                  doLogFlush,
		},
		ioThreshold: ioThreshold,
	}
}

// adjustTokensResult encapsulates all the numerical state of ioLoadListener.
type adjustTokensResult struct {
	ioLoadListenerState
	requestEstimates storeRequestEstimates
	l0WriteLM        tokensLinearModel
	l0IngestLM       tokensLinearModel
	ingestLM         tokensLinearModel
	aux              adjustTokensAuxComputations
	ioThreshold      *admissionpb.IOThreshold // never nil
}

func (res adjustTokensResult) SafeFormat(p redact.SafePrinter, _ rune) {
	ib := humanizeutil.IBytes
	// NB: "≈" indicates smoothed quantities.
	p.Printf("compaction score %v (%d ssts, %d sub-levels), ", res.ioThreshold, res.ioThreshold.L0NumFiles, res.ioThreshold.L0NumSubLevels)
	p.Printf("L0 growth %s (write %s ingest %s ignored %s): ", ib(res.aux.intL0AddedBytes),
		ib(res.aux.perWorkTokensAux.intL0WriteBytes), ib(res.aux.perWorkTokensAux.intL0IngestedBytes),
		ib(res.aux.perWorkTokensAux.intL0IgnoredIngestedBytes))
	// Writes to L0 that we expected because requests told admission control.
	// This is the "easy path", from an estimation perspective, if all regular
	// writes accurately tell us what they write, and all ingests tell us what
	// they ingest and all of ingests into L0.
	p.Printf("requests %d (%d bypassed) with ", res.aux.perWorkTokensAux.intWorkCount,
		res.aux.perWorkTokensAux.intBypassedWorkCount)
	p.Printf("%s acc-write (%s bypassed) + ",
		ib(res.aux.perWorkTokensAux.intL0WriteAccountedBytes),
		ib(res.aux.perWorkTokensAux.intL0WriteBypassedAccountedBytes))
	// Ingestion bytes that we expected because requests told admission control.
	p.Printf("%s acc-ingest (%s bypassed) + ",
		ib(res.aux.perWorkTokensAux.intIngestedAccountedBytes),
		ib(res.aux.perWorkTokensAux.intIngestedBypassedAccountedBytes))
	// The models we are fitting to compute tokens based on the reported size of
	// the write and ingest.
	p.Printf("write-model %.2fx+%s (smoothed %.2fx+%s) + ",
		res.aux.perWorkTokensAux.intL0WriteLinearModel.multiplier,
		ib(res.aux.perWorkTokensAux.intL0WriteLinearModel.constant),
		res.l0WriteLM.multiplier, ib(res.l0WriteLM.constant))
	p.Printf("ingested-model %.2fx+%s (smoothed %.2fx+%s) + ",
		res.aux.perWorkTokensAux.intL0IngestedLinearModel.multiplier,
		ib(res.aux.perWorkTokensAux.intL0IngestedLinearModel.constant),
		res.l0IngestLM.multiplier, ib(res.l0IngestLM.constant))
	// The tokens used per request at admission time, when no size information
	// is known.
	p.Printf("at-admission-tokens %s, ", ib(res.requestEstimates.writeTokens))
	// How much got compacted out of L0 recently.
	p.Printf("compacted %s [≈%s], ", ib(res.aux.intL0CompactedBytes), ib(res.smoothedIntL0CompactedBytes))
	// The tokens computed for flush, based on observed flush throughput and
	// utilization.
	p.Printf("flushed %s [≈%s]; ", ib(int64(res.aux.intFlushTokens)),
		ib(int64(res.smoothedNumFlushTokens)))
	p.Printf("admitting ")
	if n, m := res.ioLoadListenerState.totalNumByteTokens,
		res.ioLoadListenerState.totalNumElasticByteTokens; n < unlimitedTokens {
		p.Printf("%s (rate %s/s) (elastic %s rate %s/s)", ib(n), ib(n/adjustmentInterval), ib(m),
			ib(m/adjustmentInterval))
		switch res.aux.tokenKind {
		case compactionTokenKind:
			// NB: res.smoothedCompactionByteTokens  is the same as
			// res.ioLoadListenerState.totalNumByteTokens (printed above) when
			// res.aux.tokenKind == compactionTokenKind.
			p.Printf(" due to L0 growth")
		case flushTokenKind:
			p.Printf(" due to memtable flush (multiplier %.3f)", res.flushUtilTargetFraction)
		}
		p.Printf(" (used total: %s elastic %s)", ib(res.aux.prevTokensUsed),
			ib(res.aux.prevTokensUsedByElasticWork))
	} else if m < unlimitedTokens {
		p.Printf("elastic %s (rate %s/s) due to L0 growth", ib(m), ib(m/adjustmentInterval))
	} else {
		p.SafeString("all")
	}
	if res.elasticDiskBWTokens != unlimitedTokens {
		p.Printf("; elastic-disk-bw tokens %s (used %s, regular used %s): "+
			"write model %.2fx+%s ingest model %.2fx+%s, ",
			ib(res.elasticDiskBWTokens), ib(res.aux.diskBW.intervalLSMInfo.elasticTokensUsed),
			ib(res.aux.diskBW.intervalLSMInfo.regularTokensUsed),
			res.l0WriteLM.multiplier, ib(res.l0WriteLM.constant),
			res.ingestLM.multiplier, ib(res.ingestLM.constant))
		p.Printf("disk bw read %s write %s provisioned %s",
			ib(res.aux.diskBW.intervalDiskLoadInfo.readBandwidth),
			ib(res.aux.diskBW.intervalDiskLoadInfo.writeBandwidth),
			ib(res.aux.diskBW.intervalDiskLoadInfo.provisionedBandwidth))
	}
	p.Printf("; write stalls %d", res.aux.intWriteStalls)
}

func (res adjustTokensResult) String() string {
	return redact.StringWithoutMarkers(res)
}

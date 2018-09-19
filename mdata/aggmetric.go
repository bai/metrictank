package mdata

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/grafana/metrictank/cluster"
	"github.com/grafana/metrictank/conf"
	"github.com/grafana/metrictank/consolidation"
	"github.com/grafana/metrictank/mdata/cache"
	"github.com/grafana/metrictank/mdata/chunk"
	"github.com/raintank/schema"
	log "github.com/sirupsen/logrus"
)

var ErrInvalidRange = errors.New("AggMetric: invalid range: from must be less than to")
var ErrNilChunk = errors.New("AggMetric: unexpected nil chunk")

// AggMetric takes in new values, updates the in-memory data and streams the points to aggregators
// it uses a circular buffer of chunks
// each chunk starts at their respective t0
// a t0 is a timestamp divisible by chunkSpan without a remainder (e.g. 2 hour boundaries)
// firstT0's data is held at index 0, indexes go up and wrap around from numChunks-1 to 0
// in addition, keep in mind that the last chunk is always a work in progress and not useable for aggregation
// AggMetric is concurrency-safe
type AggMetric struct {
	store       Store
	cachePusher cache.CachePusher
	sync.RWMutex
	Key             schema.AMKey
	rob             *ReorderBuffer
	CurrentChunkPos int    // element in []Chunks that is active. All others are either finished or nil.
	NumChunks       uint32 // max size of the circular buffer
	ChunkSpan       uint32 // span of individual chunks in seconds
	Chunks          []*chunk.Chunk
	aggregators     []*Aggregator
	dropFirstChunk  bool
	ttl             uint32
	lastSaveStart   uint32 // last chunk T0 that was added to the write Queue.
	lastSaveFinish  uint32 // last chunk T0 successfully written to Cassandra.
	lastWrite       uint32
	firstTs         uint32
}

// NewAggMetric creates a metric with given key, it retains the given number of chunks each chunkSpan seconds long
// it optionally also creates aggregations with the given settings
// the 0th retention is the native archive of this metric. if there's several others, we create aggregators, using agg.
// it's the callers responsibility to make sure agg is not nil in that case!
func NewAggMetric(store Store, cachePusher cache.CachePusher, key schema.AMKey, retentions conf.Retentions, reorderWindow uint32, agg *conf.Aggregation, dropFirstChunk bool) *AggMetric {

	// note: during parsing of retentions, we assure there's at least 1.
	ret := retentions[0]

	m := AggMetric{
		cachePusher:    cachePusher,
		store:          store,
		Key:            key,
		ChunkSpan:      ret.ChunkSpan,
		NumChunks:      ret.NumChunks,
		Chunks:         make([]*chunk.Chunk, 0, ret.NumChunks),
		dropFirstChunk: dropFirstChunk,
		ttl:            uint32(ret.MaxRetention()),
		// we set LastWrite here to make sure a new Chunk doesn't get immediately
		// garbage collected right after creating it, before we can push to it.
		lastWrite: uint32(time.Now().Unix()),
	}
	if reorderWindow != 0 {
		m.rob = NewReorderBuffer(reorderWindow, ret.SecondsPerPoint)
	}

	for _, ret := range retentions[1:] {
		m.aggregators = append(m.aggregators, NewAggregator(store, cachePusher, key, ret, *agg, dropFirstChunk))
	}

	return &m
}

// Sync the saved state of a chunk by its T0.
func (a *AggMetric) SyncChunkSaveState(ts uint32) {
	a.Lock()
	defer a.Unlock()
	if ts > a.lastSaveFinish {
		a.lastSaveFinish = ts
	}
	if ts > a.lastSaveStart {
		a.lastSaveStart = ts
	}
	if LogLevel < 2 {
		log.Debugf("AM: metric %s at chunk T0=%d has been saved.", a.Key, ts)
	}
}

// Sync the saved state of a chunk by its T0.
func (a *AggMetric) SyncAggregatedChunkSaveState(ts uint32, consolidator consolidation.Consolidator, aggSpan uint32) {
	// no lock needed cause aggregators don't change at runtime
	for _, a := range a.aggregators {
		if a.span == aggSpan {
			switch consolidator {
			case consolidation.None:
				panic("cannot get an archive for no consolidation")
			case consolidation.Avg:
				panic("avg consolidator has no matching Archive(). you need sum and cnt")
			case consolidation.Cnt:
				if a.cntMetric != nil {
					a.cntMetric.SyncChunkSaveState(ts)
				}
				return
			case consolidation.Min:
				if a.minMetric != nil {
					a.minMetric.SyncChunkSaveState(ts)
				}
				return
			case consolidation.Max:
				if a.maxMetric != nil {
					a.maxMetric.SyncChunkSaveState(ts)
				}
				return
			case consolidation.Sum:
				if a.sumMetric != nil {
					a.sumMetric.SyncChunkSaveState(ts)
				}
				return
			case consolidation.Lst:
				if a.lstMetric != nil {
					a.lstMetric.SyncChunkSaveState(ts)
				}
				return
			default:
				panic(fmt.Sprintf("internal error: no such consolidator %q with span %d", consolidator, aggSpan))
			}
		}
	}
}

func (a *AggMetric) getChunk(pos int) *chunk.Chunk {
	if pos < 0 || pos >= len(a.Chunks) {
		panic(fmt.Sprintf("aggmetric %s queried for chunk %d out of %d chunks", a.Key, pos, len(a.Chunks)))
	}
	return a.Chunks[pos]
}

func (a *AggMetric) GetAggregated(consolidator consolidation.Consolidator, aggSpan, from, to uint32) (Result, error) {
	// no lock needed cause aggregators don't change at runtime
	for _, a := range a.aggregators {
		if a.span == aggSpan {
			var agg *AggMetric
			switch consolidator {
			case consolidation.None:
				err := errors.New("internal error: AggMetric.GetAggregated(): cannot get an archive for no consolidation")
				log.Errorf("AM: %s", err.Error())
				badConsolidator.Inc()
				return Result{}, err
			case consolidation.Avg:
				err := errors.New("internal error: AggMetric.GetAggregated(): avg consolidator has no matching Archive(). you need sum and cnt")
				log.Errorf("AM: %s", err.Error())
				badConsolidator.Inc()
				return Result{}, err
			case consolidation.Cnt:
				agg = a.cntMetric
			case consolidation.Lst:
				agg = a.lstMetric
			case consolidation.Min:
				agg = a.minMetric
			case consolidation.Max:
				agg = a.maxMetric
			case consolidation.Sum:
				agg = a.sumMetric
			default:
				err := fmt.Errorf("internal error: AggMetric.GetAggregated(): unknown consolidator %q", consolidator)
				log.Errorf("AM: %s", err.Error())
				badConsolidator.Inc()
				return Result{}, err
			}
			if agg == nil {
				return Result{}, fmt.Errorf("Consolidator %q not configured", consolidator)
			}
			return agg.Get(from, to)
		}
	}
	err := fmt.Errorf("internal error: AggMetric.GetAggregated(): unknown aggSpan %d", aggSpan)
	log.Errorf("AM: %s", err.Error())
	badAggSpan.Inc()
	return Result{}, err
}

// Get all data between the requested time ranges. From is inclusive, to is exclusive. from <= x < to
// more data then what's requested may be included
// specifically, returns:
// * points from the ROB (if enabled)
// * iters from matching chunks
// * oldest point we have, so that if your query needs data before it, the caller knows when to query the store
func (a *AggMetric) Get(from, to uint32) (Result, error) {
	pre := time.Now()
	if LogLevel < 2 {
		log.Debugf("AM: %s Get(): %d - %d (%s - %s) span:%ds", a.Key, from, to, TS(from), TS(to), to-from-1)
	}
	if from >= to {
		return Result{}, ErrInvalidRange
	}
	a.RLock()
	defer a.RUnlock()

	result := Result{
		Oldest: math.MaxInt32,
	}

	if a.rob != nil {
		result.Points = a.rob.Get()
		if len(result.Points) > 0 {
			result.Oldest = result.Points[0].Ts
			if result.Oldest <= from {
				return result, nil
			}
		}
	}

	if len(a.Chunks) == 0 {
		// we dont have any data yet.
		if LogLevel < 2 {
			log.Debugf("AM: %s Get(): no data for requested range.", a.Key)
		}
		return result, nil
	}

	newestChunk := a.getChunk(a.CurrentChunkPos)

	if from >= newestChunk.T0+a.ChunkSpan {
		// request falls entirely ahead of the data we have
		// this can happen in a few cases:
		// * queries for the most recent data, but our ingestion has fallen behind.
		//   we don't want such a degradation to cause a storm of cassandra queries
		//   we should just return an oldest value that is <= from so we don't hit cassandra
		//   but it doesn't have to be the real oldest value, so do whatever is cheap.
		// * data got once written by another node, but has been re-sending (perhaps fixed)
		//   to this node, but not yet up to the point it was previously sent. so we're
		//   only aware of older data and not the newer data in cassandra. this is unlikely
		//   and it's better to not serve this scenario well in favor of the above case.
		//   seems like a fair tradeoff anyway that you have to refill all the way first.
		if LogLevel < 2 {
			log.Debugf("AM: %s Get(): no data for requested range.", a.Key)
		}
		result.Oldest = from
		return result, nil
	}

	// get the oldest chunk we have.
	// eg if we have 5 chunks, N is the current chunk and n-4 is the oldest chunk.
	// -----------------------------
	// | n-4 | n-3 | n-2 | n-1 | n |  CurrentChunkPos = 4
	// -----------------------------
	// -----------------------------
	// | n | n-4 | n-3 | n-2 | n-1 |  CurrentChunkPos = 0
	// -----------------------------
	// -----------------------------
	// | n-2 | n-1 | n | n-4 | n-3 |  CurrentChunkPos = 2
	// -----------------------------
	oldestPos := a.CurrentChunkPos + 1
	if oldestPos >= len(a.Chunks) {
		oldestPos = 0
	}

	oldestChunk := a.getChunk(oldestPos)
	if oldestChunk == nil {
		log.Errorf("%s", ErrNilChunk)
		return result, ErrNilChunk
	}

	if to <= oldestChunk.T0 {
		// the requested time range ends before any data we have.
		if LogLevel < 2 {
			log.Debugf("AM: %s Get(): no data for requested range", a.Key)
		}
		if oldestChunk.First {
			result.Oldest = a.firstTs
		} else {
			result.Oldest = oldestChunk.T0
		}
		return result, nil
	}

	// Find the oldest Chunk that the "from" ts falls in.  If from extends before the oldest
	// chunk, then we just use the oldest chunk.
	for from >= oldestChunk.T0+a.ChunkSpan {
		oldestPos++
		if oldestPos >= len(a.Chunks) {
			oldestPos = 0
		}
		oldestChunk = a.getChunk(oldestPos)
		if oldestChunk == nil {
			result.Oldest = to
			log.Error(ErrNilChunk.Error())
			return result, ErrNilChunk
		}
	}

	// find the newest Chunk that "to" falls in.  If "to" extends to after the newest data
	// then just return the newest chunk.
	// some examples to clarify this more. assume newestChunk.T0 is at 120, then
	// for a to of 121 -> data up to (incl) 120 -> stay at this chunk, it has a point we need
	// for a to of 120 -> data up to (incl) 119 -> use older chunk
	// for a to of 119 -> data up to (incl) 118 -> use older chunk
	newestPos := a.CurrentChunkPos
	for to <= newestChunk.T0 {
		newestPos--
		if newestPos < 0 {
			newestPos += len(a.Chunks)
		}
		newestChunk = a.getChunk(newestPos)
		if newestChunk == nil {
			result.Oldest = to
			log.Error(ErrNilChunk.Error())
			return result, ErrNilChunk
		}
	}

	// now just start at oldestPos and move through the Chunks circular Buffer to newestPos
	for {
		c := a.getChunk(oldestPos)
		result.Iters = append(result.Iters, chunk.NewIter(c.Iter()))

		if oldestPos == newestPos {
			break
		}

		oldestPos++
		if oldestPos >= len(a.Chunks) {
			oldestPos = 0
		}
	}

	if oldestChunk.First {
		result.Oldest = a.firstTs
	} else {
		result.Oldest = oldestChunk.T0
	}

	memToIterDuration.Value(time.Now().Sub(pre))
	return result, nil
}

// this function must only be called while holding the lock
func (a *AggMetric) addAggregators(ts uint32, val float64) {
	for _, agg := range a.aggregators {
		if LogLevel < 2 {
			log.Debugf("AM: %s pushing %d,%f to aggregator %d", a.Key, ts, val, agg.span)
		}
		agg.Add(ts, val)
	}
}

func (a *AggMetric) pushToCache(c *chunk.Chunk) {
	if a.cachePusher == nil {
		return
	}
	// push into cache
	go a.cachePusher.AddIfHot(
		a.Key,
		0,
		*chunk.NewBareIterGen(
			c.Bytes(),
			c.T0,
			a.ChunkSpan,
		),
	)
}

// write a chunk to persistent storage. This should only be called while holding a.Lock()
func (a *AggMetric) persist(pos int) {
	chunk := a.Chunks[pos]
	pre := time.Now()

	if a.lastSaveStart >= chunk.T0 {
		// this can happen if
		// a) there are 2 primary MT nodes both saving chunks to Cassandra
		// b) a primary failed and this node was promoted to be primary but metric consuming is lagging.
		// c) chunk was persisted by GC (stale) and then new data triggered another persist call
		// d) dropFirstChunk is enabled and this is the first chunk
		log.Debugf("AM: persist(): duplicate persist call for chunk.")
		return
	}

	// create an array of chunks that need to be sent to the writeQueue.
	pending := make([]*ChunkWriteRequest, 1)
	// add the current chunk to the list of chunks to send to the writeQueue
	pending[0] = &ChunkWriteRequest{
		Metric:    a,
		Key:       a.Key,
		Span:      a.ChunkSpan,
		TTL:       a.ttl,
		Chunk:     chunk,
		Timestamp: time.Now(),
	}

	// if we recently became the primary, there may be older chunks
	// that the old primary did not save.  We should check for those
	// and save them.
	previousPos := pos - 1
	if previousPos < 0 {
		previousPos += len(a.Chunks)
	}
	previousChunk := a.Chunks[previousPos]
	for (previousChunk.T0 < chunk.T0) && (a.lastSaveStart < previousChunk.T0) {
		if LogLevel < 2 {
			log.Debugf("AM: persist(): old chunk needs saving. Adding %s:%d to writeQueue", a.Key, previousChunk.T0)
		}
		pending = append(pending, &ChunkWriteRequest{
			Metric:    a,
			Key:       a.Key,
			Span:      a.ChunkSpan,
			TTL:       a.ttl,
			Chunk:     previousChunk,
			Timestamp: time.Now(),
		})
		previousPos--
		if previousPos < 0 {
			previousPos += len(a.Chunks)
		}
		previousChunk = a.Chunks[previousPos]
	}

	// Every chunk with a T0 <= this chunks' T0 is now either saved, or in the writeQueue.
	a.lastSaveStart = chunk.T0

	if LogLevel < 2 {
		log.Debugf("AM: persist(): sending %d chunks to write queue", len(pending))
	}

	pendingChunk := len(pending) - 1

	// if the store blocks,
	// the calling function will block waiting for persist() to complete.
	// This is intended to put backpressure on our message handlers so
	// that they stop consuming messages, leaving them to buffer at
	// the message bus. The "pending" array of chunks are processed
	// last-to-first ensuring that older data is added to the store
	// before newer data.
	for pendingChunk >= 0 {
		if LogLevel < 2 {
			log.Debugf("AM: persist(): sealing chunk %d/%d (%s:%d) and adding to write queue.", pendingChunk, len(pending), a.Key, chunk.T0)
		}
		a.store.Add(pending[pendingChunk])
		pendingChunk--
	}
	persistDuration.Value(time.Now().Sub(pre))
	return
}

// don't ever call with a ts of 0, cause we use 0 to mean not initialized!
func (a *AggMetric) Add(ts uint32, val float64) {
	a.Lock()
	defer a.Unlock()

	if a.rob == nil {
		// write directly
		a.add(ts, val)
	} else {
		// write through reorder buffer
		res, accepted := a.rob.Add(ts, val)

		if len(res) == 0 && accepted {
			a.lastWrite = uint32(time.Now().Unix())
		}

		for _, p := range res {
			a.add(p.Ts, p.Val)
		}
	}
}

// don't ever call with a ts of 0, cause we use 0 to mean not initialized!
// assumes a write lock is held by the call-site
func (a *AggMetric) add(ts uint32, val float64) {
	t0 := ts - (ts % a.ChunkSpan)

	if len(a.Chunks) == 0 {
		chunkCreate.Inc()
		// no data has been added to this AggMetric yet.
		// note that we may not be aware of prior data that belongs into this chunk
		// so we should track this cutoff point
		a.Chunks = append(a.Chunks, chunk.NewFirst(t0))
		a.firstTs = ts

		if err := a.Chunks[0].Push(ts, val); err != nil {
			panic(fmt.Sprintf("FATAL ERROR: this should never happen. Pushing initial value <%d,%f> to new chunk at pos 0 failed: %q", ts, val, err))
		}

		log.Debugf("AM: %s Add(): created first chunk with first point: %v", a.Key, a.Chunks[0])
		a.lastWrite = uint32(time.Now().Unix())
		if a.dropFirstChunk {
			a.lastSaveStart = t0
			a.lastSaveFinish = t0
		}
		a.addAggregators(ts, val)
		return
	}

	currentChunk := a.getChunk(a.CurrentChunkPos)

	if t0 == currentChunk.T0 {
		// last prior data was in same chunk as new point
		if currentChunk.Closed {
			// if we've already 'finished' the chunk, it means it has the end-of-stream marker and any new points behind it wouldn't be read by an iterator
			// you should monitor this metric closely, it indicates that maybe your GC settings don't match how you actually send data (too late)
			addToClosedChunk.Inc()
			return
		}

		if err := currentChunk.Push(ts, val); err != nil {
			log.Debugf("AM: failed to add metric to chunk for %s. %s", a.Key, err)
			metricsTooOld.Inc()
			return
		}
		a.lastWrite = uint32(time.Now().Unix())
		log.Debugf("AM: %s Add(): pushed new value to last chunk: %v", a.Key, a.Chunks[0])
	} else if t0 < currentChunk.T0 {
		log.Debugf("AM: Point at %d has t0 %d, goes back into previous chunk. CurrentChunk t0: %d, LastTs: %d", ts, t0, currentChunk.T0, currentChunk.LastTs)
		metricsTooOld.Inc()
		return
	} else {
		// Data belongs in a new chunk.

		//  If it isnt finished already, add the end-of-stream marker and flag the chunk as "closed"
		if !currentChunk.Closed {
			currentChunk.Finish()
		}

		a.pushToCache(currentChunk)
		// If we are a primary node, then add the chunk to the write queue to be saved to Cassandra
		if cluster.Manager.IsPrimary() {
			if LogLevel < 2 {
				log.Debugf("AM: persist(): node is primary, saving chunk. %s T0: %d", a.Key, currentChunk.T0)
			}
			// persist the chunk. If the writeQueue is full, then this will block.
			a.persist(a.CurrentChunkPos)
		}

		a.CurrentChunkPos++
		if a.CurrentChunkPos >= int(a.NumChunks) {
			a.CurrentChunkPos = 0
		}

		chunkCreate.Inc()
		if len(a.Chunks) < int(a.NumChunks) {
			a.Chunks = append(a.Chunks, chunk.New(t0))
			if err := a.Chunks[a.CurrentChunkPos].Push(ts, val); err != nil {
				panic(fmt.Sprintf("FATAL ERROR: this should never happen. Pushing initial value <%d,%f> to new chunk at pos %d failed: %q", ts, val, a.CurrentChunkPos, err))
			}
			log.Debugf("AM: %s Add(): added new chunk to buffer. now %d chunks. and added the new point: %s", a.Key, a.CurrentChunkPos+1, a.Chunks[a.CurrentChunkPos])
		} else {
			chunkClear.Inc()
			a.Chunks[a.CurrentChunkPos].Clear()
			a.Chunks[a.CurrentChunkPos] = chunk.New(t0)
			if err := a.Chunks[a.CurrentChunkPos].Push(ts, val); err != nil {
				panic(fmt.Sprintf("FATAL ERROR: this should never happen. Pushing initial value <%d,%f> to new chunk at pos %d failed: %q", ts, val, a.CurrentChunkPos, err))
			}
			log.Debugf("AM: %s Add(): cleared chunk at %d of %d and replaced with new. and added the new point: %s", a.Key, a.CurrentChunkPos, len(a.Chunks), a.Chunks[a.CurrentChunkPos])
		}
		a.lastWrite = uint32(time.Now().Unix())

	}
	a.addAggregators(ts, val)
}

// collectable returns whether the AggMetric is garbage collectable
// an Aggmetric is collectable based on two conditions:
// * the AggMetric hasn't been written to in a configurable amount of time
//   (wether the write went to the ROB or a chunk is irrelevant)
// * the last chunk - if any - is no longer "active".
//   active means:
//   any reasonable realtime stream (e.g. up to 15 min behind wall-clock)
//   could add points to the chunk
//
// caller must hold AggMetric lock
func (a *AggMetric) collectable(now, chunkMinTs uint32) bool {

	var currentChunk *chunk.Chunk
	if len(a.Chunks) != 0 {
		currentChunk = a.getChunk(a.CurrentChunkPos)
	}

	// no chunks at all means "possibly collectable"
	// the caller (AggMetric.GC()) still has its own checks to
	// handle the "no chunks" correctly later.
	// also: we want AggMetric.GC() to go ahead with flushing the ROB in this case
	if currentChunk == nil {
		return a.lastWrite < chunkMinTs
	}

	return a.lastWrite < chunkMinTs && currentChunk.Series.T0+a.ChunkSpan+15*60 < now
}

// GC returns whether or not this AggMetric is stale and can be removed
// chunkMinTs -> min timestamp of a chunk before to be considered stale and to be persisted to Cassandra
// metricMinTs -> min timestamp for a metric before to be considered stale and to be purged from the tank
func (a *AggMetric) GC(now, chunkMinTs, metricMinTs uint32) bool {
	a.Lock()
	defer a.Unlock()

	// abort unless it looks like the AggMetric is collectable
	if !a.collectable(now, chunkMinTs) {
		return false
	}

	// make sure any points in the reorderBuffer are moved into our chunks so we can save the data
	if a.rob != nil {
		tmpLastWrite := a.lastWrite
		pts := a.rob.Flush()
		for _, p := range pts {
			a.add(p.Ts, p.Val)
		}

		// adding points will cause our lastWrite to be updated, but we want to keep the old value
		a.lastWrite = tmpLastWrite
	}

	// this aggMetric has never had metrics written to it.
	if len(a.Chunks) == 0 {
		return a.gcAggregators(now, chunkMinTs, metricMinTs)
	}

	currentChunk := a.getChunk(a.CurrentChunkPos)
	if currentChunk == nil {
		return false
	}

	// we must check collectable again. Imagine this scenario:
	// * we didn't have any chunks when calling collectable() the first time so it returned true
	// * data from the ROB is flushed and moved into a new chunk
	// * this new chunk is active so we're not collectable, even though earlier we thought we were.
	if !a.collectable(now, chunkMinTs) {
		return false
	}

	if currentChunk.Closed {
		// already closed and should be saved, though we cant guarantee that.
		// Check if we should just delete the metric from memory.
		if a.lastWrite < metricMinTs {
			return a.gcAggregators(now, chunkMinTs, metricMinTs)
		}
	} else {
		// chunk hasn't been written to in a while, and is not yet closed. Let's close it and persist it if
		// we are a primary
		log.Debugf("AM: Found stale Chunk, adding end-of-stream bytes. key: %v T0: %d", a.Key, currentChunk.T0)
		currentChunk.Finish()
		if cluster.Manager.IsPrimary() {
			if LogLevel < 2 {
				log.Debugf("AM: persist(): node is primary, saving chunk. %v T0: %d", a.Key, currentChunk.T0)
			}
			// persist the chunk. If the writeQueue is full, then this will block.
			a.persist(a.CurrentChunkPos)
		}
	}
	return false
}

func (a *AggMetric) gcAggregators(now, chunkMinTs, metricMinTs uint32) bool {
	ret := true
	for _, agg := range a.aggregators {
		ret = agg.GC(now, chunkMinTs, metricMinTs, a.lastWrite) && ret
	}
	return ret
}

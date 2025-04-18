// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// {{/*
//go:build execgen_template

//
// This file is the execgen template for window_aggregator.eg.go. It's formatted
// in a special way, so it's both valid Go and a valid text/template input. This
// permits editing this file with editor support.
//
// */}}

package colexecwindow

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecagg"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecutils"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecop"
	"github.com/cockroachdb/cockroach/pkg/sql/colmem"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

type slidingWindowAggregateFunc interface {
	colexecagg.AggregateFunc

	// Remove removes the indicated rows from the the aggregation. It is used when
	// the window frame for the previous row included rows that are not included
	// in the current frame.
	// Note: the implementations should be careful to account for their memory
	// usage.
	// Note: endIdx is assumed to be greater than zero.
	Remove(vecs []*coldata.Vec, inputIdxs []uint32, startIdx, endIdx int)
}

// NewWindowAggregatorOperator creates a new Operator that computes aggregate
// window functions. outputColIdx specifies in which coldata.Vec the operator
// should put its output (if there is no such column, a new column is appended).
func NewWindowAggregatorOperator(
	args *WindowArgs,
	aggType execinfrapb.AggregatorSpec_Func,
	frame *execinfrapb.WindowerSpec_Frame,
	ordering *execinfrapb.Ordering,
	argIdxs []int,
	outputType *types.T,
	aggAlloc *colexecagg.AggregateFuncsAlloc,
) colexecop.ClosableOperator {
	// Because the buffer is used multiple times per-row, it is important to
	// prevent it from spilling to disk if possible. For this reason, we give the
	// buffer half of the memory budget even though it will generally store less
	// columns than the queue.
	bufferMemLimit := int64(float64(args.MemoryLimit) * 0.5)
	mainMemLimit := args.MemoryLimit - bufferMemLimit
	framer := newWindowFramer(args.EvalCtx, frame, ordering, args.InputTypes, args.PeersColIdx)
	colsToStore := framer.getColsToStore(append([]int{}, argIdxs...))
	buffer := colexecutils.NewSpillingBuffer(
		args.BufferAllocator, bufferMemLimit, args.QueueCfg, args.FdSemaphore,
		args.InputTypes, args.DiskAcc, args.DiskQueueMemAcc, colsToStore...,
	)
	inputIdxs := make([]uint32, len(argIdxs))
	for i := range inputIdxs {
		// We will always store the arg columns first in the buffer.
		inputIdxs[i] = uint32(i)
	}
	base := windowAggregatorBase{
		partitionSeekerBase: partitionSeekerBase{
			partitionColIdx: args.PartitionColIdx,
			buffer:          buffer,
		},
		allocator:    args.MainAllocator,
		outputColIdx: args.OutputColIdx,
		inputIdxs:    inputIdxs,
		framer:       framer,
		vecs:         make([]*coldata.Vec, len(inputIdxs)),
	}
	var agg colexecagg.AggregateFunc
	if aggAlloc != nil {
		agg = aggAlloc.MakeAggregateFuncs()[0]
	}
	var windower bufferedWindower
	switch aggType {
	case execinfrapb.Min, execinfrapb.Max:
		if WindowFrameCanShrink(frame, ordering) {
			// In the case when the window frame for a given row does not necessarily
			// include all rows from the previous frame, min and max require a
			// specialized implementation that maintains a dequeue of seen values.
			if frame.Exclusion != execinfrapb.WindowerSpec_Frame_NO_EXCLUSION {
				// TODO(drewk): extend the implementations to work with non-default
				// exclusion. For now, we have to use the quadratic-time method.
				windower = &windowAggregator{windowAggregatorBase: base, agg: agg}
			} else {
				switch aggType {
				case execinfrapb.Min:
					windower = newMinRemovableAggregator(args, framer, buffer, outputType)
				case execinfrapb.Max:
					windower = newMaxRemovableAggregator(args, framer, buffer, outputType)
				}
			}
		} else {
			// When the frame can only grow, the simple sliding window implementation
			// is sufficient.
			windower = &slidingWindowAggregator{
				windowAggregatorBase: base,
				agg:                  agg.(slidingWindowAggregateFunc),
			}
		}
	case execinfrapb.BoolAnd, execinfrapb.BoolOr:
		if WindowFrameCanShrink(frame, ordering) {
			// TODO(drewk): add optimized implementations that can handle a shrinking
			// window by tracking counts of true, false, null values instead of just
			// the final aggregate value.
			windower = &windowAggregator{windowAggregatorBase: base, agg: agg}
		} else {
			// These aggregates can only be used in a sliding-window context if the
			// window does not shrink.
			windower = &slidingWindowAggregator{
				windowAggregatorBase: base,
				agg:                  agg.(slidingWindowAggregateFunc),
			}
		}
	default:
		if slidingWindowAgg, ok := agg.(slidingWindowAggregateFunc); ok {
			windower = &slidingWindowAggregator{windowAggregatorBase: base, agg: slidingWindowAgg}
		} else {
			windower = &windowAggregator{windowAggregatorBase: base, agg: agg}
		}
	}
	return newBufferedWindowOperator(args, windower, outputType, mainMemLimit)
}

type windowAggregatorBase struct {
	partitionSeekerBase
	colexecop.CloserHelper
	allocator     *colmem.Allocator
	cancelChecker colexecutils.CancelChecker

	outputColIdx int
	inputIdxs    []uint32
	vecs         []*coldata.Vec
	framer       windowFramer
}

type windowAggregator struct {
	windowAggregatorBase
	agg colexecagg.AggregateFunc
}

type slidingWindowAggregator struct {
	windowAggregatorBase
	agg slidingWindowAggregateFunc
}

var (
	_ bufferedWindower = &windowAggregator{}
	_ bufferedWindower = &slidingWindowAggregator{}
)

// windowInterval represents rows in the range [start, end). Slices of
// windowIntervals should always be increasing and non-overlapping.
type windowInterval struct {
	start int
	end   int
}

// transitionToProcessing implements the bufferedWindower interface.
func (a *windowAggregatorBase) transitionToProcessing() {
	a.framer.startPartition(a.Ctx, a.partitionSize, a.buffer)
}

// startNewPartition implements the bufferedWindower interface.
func (a *windowAggregatorBase) startNewPartition() {
	a.partitionSize = 0
	a.buffer.Reset(a.Ctx)
}

// Init implements the bufferedWindower interface.
func (a *windowAggregatorBase) Init(ctx context.Context) {
	a.InitHelper.Init(ctx)
	a.cancelChecker.Init(a.Ctx)
}

// Close implements the bufferedWindower interface.
func (a *windowAggregatorBase) Close(ctx context.Context) {
	if !a.CloserHelper.Close() {
		return
	}
	a.framer.close()
	a.buffer.Close(ctx)
}

func (a *windowAggregator) startNewPartition() {
	a.windowAggregatorBase.startNewPartition()
	a.agg.Reset()
}

func (a *windowAggregator) Close(ctx context.Context) {
	a.windowAggregatorBase.Close(ctx)
	a.agg.Reset()
	*a = windowAggregator{}
}

// processBatch implements the bufferedWindower interface.
func (a *windowAggregator) processBatch(batch coldata.Batch, startIdx, endIdx int) {
	outVec := batch.ColVec(a.outputColIdx)
	a.agg.SetOutput(outVec)
	a.allocator.PerformOperation([]*coldata.Vec{outVec}, func() {
		for i := startIdx; i < endIdx; i++ {
			a.framer.next(a.Ctx)
			aggregateOverIntervals(a.framer.frameIntervals(), false /* removeRows */)
			a.agg.Flush(i)
			a.agg.Reset()
		}
	})
}

func (a *slidingWindowAggregator) startNewPartition() {
	a.windowAggregatorBase.startNewPartition()
	a.agg.Reset()
}

func (a *slidingWindowAggregator) Close(ctx context.Context) {
	a.windowAggregatorBase.Close(ctx)
	a.agg.Reset()
	*a = slidingWindowAggregator{}
}

// processBatch implements the bufferedWindower interface.
func (a *slidingWindowAggregator) processBatch(batch coldata.Batch, startIdx, endIdx int) {
	outVec := batch.ColVec(a.outputColIdx)
	a.agg.SetOutput(outVec)
	a.allocator.PerformOperation([]*coldata.Vec{outVec}, func() {
		for i := startIdx; i < endIdx; i++ {
			a.framer.next(a.Ctx)
			toAdd, toRemove := a.framer.slidingWindowIntervals()
			// Process the 'toRemove' intervals first to avoid overflowing.
			aggregateOverIntervals(toRemove, true /* removeRows */)
			aggregateOverIntervals(toAdd, false /* removeRows */)
			a.agg.Flush(i)
		}
	})
}

// INVARIANT: the rows within a window frame are always processed in the same
// order, regardless of whether the user specified an ordering. This means that
// two rows with the exact same frame will produce the same result for a given
// aggregation.
//
// execgen:inline
// execgen:template<removeRows>
func aggregateOverIntervals(intervals []windowInterval, removeRows bool) {
	for _, interval := range intervals {
		// intervalIdx maintains the index up to which the current interval has
		// already been processed.
		intervalIdx := interval.start
		start, end := interval.start, interval.end
		intervalLen := interval.end - interval.start
		for intervalLen > 0 {
			a.cancelChecker.Check()
			for j, idx := range a.inputIdxs {
				a.vecs[j], start, end = a.buffer.GetVecWithTuple(a.Ctx, int(idx), intervalIdx)
			}
			if intervalLen < (end - start) {
				// This is the last batch in the current interval.
				end = start + intervalLen
			}
			intervalIdx += end - start
			intervalLen -= end - start
			if removeRows {
				a.agg.Remove(a.vecs, a.inputIdxs, start, end)
			} else {
				a.agg.Compute(a.vecs, a.inputIdxs, start, end, nil /* sel */)
			}
		}
	}
}

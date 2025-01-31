// Copyright 2022 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package streams

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/go-kafka-event-source/streams/sak"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Returned by an EventProcessor or Interjector in response to an EventContext. ExecutionState
// should not be conflated with concepts of error state, such as Success or Failure.
type ExecutionState int

const (
	// Complete signals the EventSource that the event or interjection is completely processed.
	// Once Complete is returned, the offset for the associated EventContext will be commited.
	Complete ExecutionState = 0
	// Incomplete signals the EventSource that the event or interjection is still ongoing, and
	// that your application promises to fulfill the EventContext in the future.
	// The offset for the associated EventContext will not be commited.
	Incomplete ExecutionState = 1

	Fatal       ExecutionState = 2
	unknownType ExecutionState = 3
)

type AsyncJob[T any] struct {
	ctx       *EventContext[T]
	finalizer func() ExecutionState
}

func (aj AsyncJob[T]) Finalize() ExecutionState {
	return aj.finalizer()
}

type asyncCompleter[T any] struct {
	asyncJobs chan AsyncJob[T]
}

func (ac asyncCompleter[T]) AsyncComplete(j AsyncJob[T]) {
	ac.asyncJobs <- j
}

type partitionWorker[T StateStore] struct {
	eosProducer            *eosProducerPool[T]
	partitionInput         chan []*kgo.Record
	maxPending             chan struct{}
	interjectionInput      chan *interjection[T]
	eventInput             chan *EventContext[T]
	interjectionEventInput chan *EventContext[T]
	asyncCompleter         asyncCompleter[T]
	stopSignal             chan struct{}
	revokedSignal          chan struct{}
	stopped                chan struct{}
	changeLog              changeLogPartition[T]
	eventSource            *EventSource[T]
	runStatus              sak.RunStatus
	ready                  int64
	highestOffset          int64
	topicPartition         TopicPartition
	revocationWaiter       sync.WaitGroup
}

func newPartitionWorker[T StateStore](
	eventSource *EventSource[T],
	topicPartition TopicPartition,
	changeLog changeLogPartition[T],
	eosProducer *eosProducerPool[T],
	waiter func()) *partitionWorker[T] {

	eosConfig := eventSource.source.config.EosConfig

	recordsInputSize := sak.Max(eosConfig.MaxBatchSize/10, 100)
	asyncSize := recordsInputSize * 4
	pw := &partitionWorker[T]{
		eventSource:    eventSource,
		topicPartition: topicPartition,
		changeLog:      changeLog,
		eosProducer:    eosProducer,
		stopSignal:     make(chan struct{}),
		revokedSignal:  make(chan struct{}, 1),
		stopped:        make(chan struct{}),
		maxPending:     make(chan struct{}, eosProducer.maxPendingItems()),
		asyncCompleter: asyncCompleter[T]{
			asyncJobs: make(chan AsyncJob[T], asyncSize),
		},
		partitionInput:         make(chan []*kgo.Record, 4),
		eventInput:             make(chan *EventContext[T], recordsInputSize),
		interjectionInput:      make(chan *interjection[T], 1),
		interjectionEventInput: make(chan *EventContext[T], 1),
		runStatus:              eventSource.runStatus.Fork(),
		highestOffset:          -1,
	}

	go pw.work(pw.eventSource.interjections, waiter)

	return pw
}
func (pw *partitionWorker[T]) canInterject() bool {
	return atomic.LoadInt64(&pw.ready) != 0
}

func (pw *partitionWorker[T]) add(records []*kgo.Record) {
	if pw.isRevoked() {
		return
	}
	// atomic.AddInt64(&pw.pending, int64(len(records)))
	pw.partitionInput <- records
}

func (pw *partitionWorker[T]) revoke() {
	pw.runStatus.Halt()
}

type sincer struct {
	then time.Time
}

func (s sincer) String() string {
	return fmt.Sprintf("%v", time.Since(s.then))
}

func (pw *partitionWorker[T]) pushRecords() {
	for {
		select {
		case records := <-pw.partitionInput:
			if !pw.isRevoked() {
				pw.scheduleTxnAndExecution(records)
			}
		case ij := <-pw.interjectionInput:
			pw.scheduleInterjection(ij)
		case <-pw.runStatus.Done():
			log.Debugf("Closing worker for %+v", pw.topicPartition)
			pw.stopSignal <- struct{}{}
			<-pw.stopped
			close(pw.partitionInput)
			close(pw.eventInput)
			close(pw.asyncCompleter.asyncJobs)
			log.Debugf("Closed worker for %+v", pw.topicPartition)
			return
		}
	}
}

func (pw *partitionWorker[T]) scheduleTxnAndExecution(records []*kgo.Record) {
	if pw.isRevoked() {
		return
	}

	pw.revocationWaiter.Add(len(records)) // optimistically do one add call
	for _, record := range records {
		if record != nil && record.Offset >= pw.highestOffset {
			ec := newEventContext(pw.runStatus.Ctx(), record, pw.changeLog.changeLogData(), pw)
			pw.maxPending <- struct{}{}
			pw.eosProducer.addEventContext(ec)
			pw.eventInput <- ec
		} else {
			pw.revocationWaiter.Done() // in the rare occasion this is a stale evetn, decrement the revocation waiter
		}
		// this is needed as, when under load, the record input may starve out interjections
		// which have a very small input buffer
		pw.interleaveInterjection()
	}
}

func (pw *partitionWorker[T]) interleaveInterjection() {
	select {
	case ij := <-pw.interjectionInput:
		pw.scheduleInterjection(ij)
	default:
	}
}

func (pw *partitionWorker[T]) scheduleInterjection(inter *interjection[T]) {
	if pw.isRevoked() {
		if inter.callback != nil {
			inter.callback()
		}
		return
	}
	pw.revocationWaiter.Add(1)
	ec := newInterjectionContext(pw.runStatus.Ctx(), inter, pw.topicPartition, pw.changeLog.changeLogData(), pw)
	pw.maxPending <- struct{}{}
	pw.eosProducer.addEventContext(ec)
	pw.interjectionEventInput <- ec
}

func (pw *partitionWorker[T]) work(interjections []interjection[T], waiter func()) {
	elapsed := sincer{time.Now()}
	// the partition is not ready to receive events as it is still bootstrapping the state store.
	// in the case where this partition was assigned due to a failure on another consumer, this could be a lengthy process
	// if we continue to consume events for this partition, we will fill it's input buffer
	// and block other partitions on this consumer. pause the partition until tghe state store is bootstrapped
	partitionMap := map[string][]int32{
		pw.topicPartition.Topic: {pw.topicPartition.Partition},
	}
	pw.eventSource.consumer.Client().PauseFetchPartitions(partitionMap)

	waiter()
	// resume partition consumption
	pw.eventSource.consumer.Client().ResumeFetchPartitions(partitionMap)

	go pw.pushRecords()
	atomic.StoreInt64(&pw.ready, 1)
	log.Debugf("partitionWorker activated %+v in %v, interjectionCount: %d", pw.topicPartition, elapsed, len(interjections))
	ijPtrs := sak.ToPtrSlice(interjections)
	for _, ij := range ijPtrs {
		ij.init(pw.topicPartition, pw.interjectionInput)
		ij.tick()
	}
	pw.eventSource.source.onPartitionActivated(pw.topicPartition.Partition)
	for {
		select {
		case ec := <-pw.eventInput:
			pw.handleEvent(ec)
		case ec := <-pw.interjectionEventInput:
			pw.handleInterjection(ec)
		case job := <-pw.asyncCompleter.asyncJobs:
			pw.processAsyncJob(job)
		case <-pw.stopSignal:
			for _, ij := range ijPtrs {
				ij.cancel()
			}
			go pw.waitForRevocation()
		case <-pw.revokedSignal:
			pw.stopped <- struct{}{}
			return
		}
	}
}

func (pw *partitionWorker[T]) waitForRevocation() {
	pw.revocationWaiter.Wait() // wait until all pending events have been accpted by a producerNode
	pw.revokedSignal <- struct{}{}
}

func (pw *partitionWorker[T]) processAsyncJob(job AsyncJob[T]) {
	if job.Finalize() == Complete {
		job.ctx.complete()
		<-pw.maxPending
	}
}

func (pw *partitionWorker[T]) isRevoked() bool {
	return !pw.runStatus.Running()
}

func (pw *partitionWorker[T]) handleInterjection(ec *EventContext[T]) {
	inter := ec.interjection
	pw.assignProducer(ec)
	if ec.producer == nil {
		<-pw.maxPending
		if inter.callback != nil {
			inter.callback() // we need to close off 1-off interjections to prevent sourceConsumer from hanging
		}
	} else if ec.producer != nil && inter.interject(ec) == Complete {
		ec.complete()
		<-pw.maxPending
		inter.tick()
	}
}

func (pw *partitionWorker[T]) handleEvent(ec *EventContext[T]) bool {
	pw.forwardToEventSource(ec)
	return true
}

func (pw *partitionWorker[T]) assignProducer(ec *EventContext[T]) {
	// if we stop processing async completions while waiting for a producer
	// we could eventually dealock with the eos producer
	// if nothing is yet available, go ahead and process an asyncJob
	for {
		select {
		case ec.producer = <-ec.producerChan:
			return
		case job := <-pw.asyncCompleter.asyncJobs:
			pw.processAsyncJob(job)
		}
	}
}

func (pw *partitionWorker[T]) forwardToEventSource(ec *EventContext[T]) {
	pw.assignProducer(ec)
	if ec.producer == nil {
		// if we're revoked, don't even add this to the onDeck producer
		<-pw.maxPending
		return
	}
	offset := ec.Offset()
	pw.highestOffset = offset + 1
	record, _ := ec.Input()
	if pw.eventSource.handleEvent(ec, record) == Complete {
		ec.complete()
		<-pw.maxPending
	}
}

// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Radu Berinde (radu@cockroachlabs.com)

package distsqlrun

import (
	"io"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/mon"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

// Version identifies the distsqlrun protocol version.
//
// This version is separate from the main CockroachDB version numbering; it is
// only changed when the distsqlrun API changes.
//
// The planner populates the version in SetupFlowRequest.
// A server only accepts requests with versions in the range MinAcceptedVersion
// to Version.
//
// Is is possible used to provide a "window" of compatibility when new features are
// added. Example:
//  - we start with Version=1; distsqlrun servers with version 1 only accept
//    requests with version 1.
//  - a new distsqlrun feature is added; Version is bumped to 2. The
//    planner does not yet use this feature by default; it still issues
//    requests with version 1.
//  - MinAcceptedVersion is still 1, i.e. servers with version 2
//    accept both versions 1 and 2.
//  - after an upgrade cycle, we can enable the feature in the planner,
//    requiring version 2.
//  - at some later point, we can choose to deprecate version 1 and have
//    servers only accept versions >= 2 (by setting
//    MinAcceptedVersion to 2).
const Version = 4

// MinAcceptedVersion is the oldest version that the server is
// compatible with; see above.
const MinAcceptedVersion = 4

var distSQLUseTempStorage = settings.RegisterBoolSetting(
	"sql.defaults.distsql.tempstorage",
	"set to true to enable use of disk for larger distributed sql queries",
	false,
)

var noteworthyMemoryUsageBytes = envutil.EnvOrDefaultInt64("COCKROACH_NOTEWORTHY_DISTSQL_MEMORY_USAGE", 10*1024)

// ServerConfig encompasses the configuration required to create a
// DistSQLServer.
type ServerConfig struct {
	log.AmbientContext

	// DB is a handle to the cluster.
	DB *client.DB
	// FlowDB is the DB that flows should use for interacting with the database.
	// This DB has to be set such that it bypasses the local TxnCoordSender. We
	// want only the TxnCoordSender on the gateway to be involved with requests
	// performed by DistSQL.
	FlowDB       *client.DB
	RPCContext   *rpc.Context
	Stopper      *stop.Stopper
	TestingKnobs TestingKnobs

	ParentMemoryMonitor *mon.MemoryMonitor
	Counter             *metric.Counter
	Hist                *metric.Histogram

	// TempStorage is used by some DistSQL processors to store rows when the
	// working set is larger than can be stored in memory. It can be nil, if this
	// cockroach node does not have an engine for temporary storage.
	TempStorage engine.Engine

	// NodeID is the id of the node on which this Server is running.
	NodeID    *base.NodeIDContainer
	ClusterID uuid.UUID
}

// TempStorageIDGenerator generates unique IDs for each processor to use as a
// unique (unique to this node, on this uptime) prefix when writing to temp
// storage.
type TempStorageIDGenerator struct {
	nextID uint64
}

// NewID generates a new unique ID.
func (t *TempStorageIDGenerator) NewID() uint64 {
	return atomic.AddUint64(&t.nextID, 1)
}

// ServerImpl implements the server for the distributed SQL APIs.
type ServerImpl struct {
	ServerConfig
	flowRegistry  *flowRegistry
	flowScheduler *flowScheduler
	memMonitor    mon.MemoryMonitor
	regexpCache   *parser.RegexpCache
	// tempStorage is used by some DistSQL processors to store working sets
	// larger than memory. It can be nil, in which case processors should still
	// gracefully OOM if the working set gets too large.
	tempStorage engine.Engine
	// tempStorageIDGenerator is used to generate unique prefixes per processor so that
	// each processor uses a nonoverlapping part of the temp keyspace.
	tempStorageIDGenerator TempStorageIDGenerator
}

var _ DistSQLServer = &ServerImpl{}

// NewServer instantiates a DistSQLServer.
func NewServer(ctx context.Context, cfg ServerConfig) *ServerImpl {
	ds := &ServerImpl{
		ServerConfig:  cfg,
		regexpCache:   parser.NewRegexpCache(512),
		flowRegistry:  makeFlowRegistry(),
		flowScheduler: newFlowScheduler(cfg.AmbientContext, cfg.Stopper),
		memMonitor: mon.MakeMonitor("distsql",
			cfg.Counter, cfg.Hist, -1 /* increment: use default block size */, noteworthyMemoryUsageBytes),
		tempStorage:            cfg.TempStorage,
		tempStorageIDGenerator: TempStorageIDGenerator{},
	}
	ds.memMonitor.Start(ctx, cfg.ParentMemoryMonitor, mon.BoundAccount{})
	return ds
}

// Start launches workers for the server.
func (ds *ServerImpl) Start() {
	ds.flowScheduler.Start()
}

// Note: unless an error is returned, the returned context contains a span that
// must be finished through Flow.Cleanup.
func (ds *ServerImpl) setupFlow(
	ctx context.Context,
	parentSpan opentracing.Span,
	req *SetupFlowRequest,
	syncFlowConsumer RowReceiver,
) (context.Context, *Flow, error) {
	if req.Version < MinAcceptedVersion ||
		req.Version > Version {
		err := errors.Errorf(
			"version mismatch in flow request: %d; this node accepts %d through %d",
			req.Version, MinAcceptedVersion, Version,
		)
		log.Warning(ctx, err)
		return ctx, nil, err
	}
	nodeID := ds.ServerConfig.NodeID.Get()
	if nodeID == 0 {
		return nil, nil, errors.Errorf("setupFlow called before the NodeID was resolved")
	}

	const opName = "flow"
	var sp opentracing.Span
	if parentSpan == nil {
		sp = ds.Tracer.StartSpan(opName)
	} else {
		// We use FollowsFrom because the flow's span outlives the SetupFlow request.
		sp = ds.Tracer.StartSpan(opName, opentracing.FollowsFrom(parentSpan.Context()))
	}
	ctx = opentracing.ContextWithSpan(ctx, sp)

	// The monitor and account opened here are closed in Flow.Cleanup().
	monitor := mon.MakeMonitor("flow",
		ds.Counter, ds.Hist, -1 /* use default block size */, noteworthyMemoryUsageBytes)
	monitor.Start(ctx, &ds.memMonitor, mon.BoundAccount{})
	acc := monitor.MakeBoundAccount()

	location, err := sqlbase.TimeZoneStringToLocation(req.EvalContext.Location)
	if err != nil {
		tracing.FinishSpan(sp)
		return ctx, nil, err
	}
	evalCtx := parser.EvalContext{
		Location:     &location,
		Database:     req.EvalContext.Database,
		SearchPath:   parser.SearchPath(req.EvalContext.SearchPath),
		ClusterID:    ds.ServerConfig.ClusterID,
		NodeID:       nodeID,
		ReCache:      ds.regexpCache,
		Mon:          &monitor,
		ActiveMemAcc: &acc,
		Ctx: func() context.Context {
			// TODO(andrei): This is wrong. Each processor should override Ctx with its
			// own context.
			return ctx
		},
	}
	evalCtx.SetStmtTimestamp(time.Unix(0 /* sec */, req.EvalContext.StmtTimestampNanos))
	evalCtx.SetTxnTimestamp(time.Unix(0 /* sec */, req.EvalContext.TxnTimestampNanos))
	evalCtx.SetClusterTimestamp(req.EvalContext.ClusterTimestamp)

	// TODO(radu): we should sanity check some of these fields (especially
	// txnProto).
	flowCtx := FlowCtx{
		AmbientContext:         ds.AmbientContext,
		stopper:                ds.Stopper,
		id:                     req.Flow.FlowID,
		evalCtx:                evalCtx,
		rpcCtx:                 ds.RPCContext,
		txnProto:               &req.Txn,
		clientDB:               ds.DB,
		remoteTxnDB:            ds.FlowDB,
		testingKnobs:           ds.TestingKnobs,
		nodeID:                 nodeID,
		tempStorageIDGenerator: &ds.tempStorageIDGenerator,
		tempStorage:            ds.tempStorage,
	}

	ctx = flowCtx.AnnotateCtx(ctx)

	f := newFlow(flowCtx, ds.flowRegistry, syncFlowConsumer)
	flowCtx.AddLogTagStr("f", f.id.Short())
	if err := f.setup(ctx, &req.Flow); err != nil {
		log.Errorf(ctx, "error setting up flow: %s", err)
		tracing.FinishSpan(sp)
		ctx = opentracing.ContextWithSpan(ctx, nil)
		return ctx, nil, err
	}
	return ctx, f, nil
}

// SetupSyncFlow sets up a synchoronous flow, connecting the sync response
// output stream to the given RowReceiver. The flow is not started. The flow
// will be associated with the given context.
// Note: the returned context contains a span that must be finished through
// Flow.Cleanup.
func (ds *ServerImpl) SetupSyncFlow(
	ctx context.Context, req *SetupFlowRequest, output RowReceiver,
) (context.Context, *Flow, error) {
	return ds.setupFlow(ds.AnnotateCtx(ctx), opentracing.SpanFromContext(ctx), req, output)
}

// RunSyncFlow is part of the DistSQLServer interface.
func (ds *ServerImpl) RunSyncFlow(stream DistSQL_RunSyncFlowServer) error {
	// Set up the outgoing mailbox for the stream.
	mbox := newOutboxSyncFlowStream(stream)

	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}
	if firstMsg.SetupFlowRequest == nil {
		return errors.Errorf("first message in RunSyncFlow doesn't contain SetupFlowRequest")
	}
	req := firstMsg.SetupFlowRequest
	ctx, f, err := ds.SetupSyncFlow(stream.Context(), req, mbox)
	if err != nil {
		return err
	}
	mbox.setFlowCtx(&f.FlowCtx)

	if err := ds.Stopper.RunTask(ctx, "distsqlrun.ServerImpl: sync flow", func(ctx context.Context) {
		f.waitGroup.Add(1)
		mbox.start(ctx, &f.waitGroup)
		f.Start(ctx, func() {})
		f.Wait()
		f.Cleanup(ctx)
	}); err != nil {
		return err
	}
	return mbox.err
}

// SetupFlow is part of the DistSQLServer interface.
func (ds *ServerImpl) SetupFlow(
	ctx context.Context, req *SetupFlowRequest,
) (*SimpleResponse, error) {
	parentSpan := opentracing.SpanFromContext(ctx)

	// Note: the passed context will be canceled when this RPC completes, so we
	// can't associate it with the flow.
	ctx = ds.AnnotateCtx(context.Background())
	ctx, f, err := ds.setupFlow(ctx, parentSpan, req, nil)
	if err == nil {
		err = ds.flowScheduler.ScheduleFlow(ctx, f)
	}
	if err != nil {
		// We return flow deployment errors in the response so that they are
		// packaged correctly over the wire. If we return them directly to this
		// function, they become part of an rpc error.
		return &SimpleResponse{Error: NewError(err)}, nil
	}
	return &SimpleResponse{}, nil
}

func (ds *ServerImpl) flowStreamInt(ctx context.Context, stream DistSQL_FlowStreamServer) error {
	// Receive the first message.
	msg, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return errors.Errorf("missing header message")
		}
		return err
	}
	if msg.Header == nil {
		return errors.Errorf("no header in first message")
	}
	flowID := msg.Header.FlowID
	streamID := msg.Header.StreamID
	if log.V(1) {
		log.Infof(ctx, "connecting inbound stream %s/%d", flowID.Short(), streamID)
	}
	f, receiver, cleanup, err := ds.flowRegistry.ConnectInboundStream(
		ctx, flowID, streamID, flowStreamDefaultTimeout)
	if err != nil {
		return err
	}
	defer cleanup()
	log.VEventf(ctx, 1, "connected inbound stream %s/%d", flowID.Short(), streamID)
	return ProcessInboundStream(f.AnnotateCtx(ctx), stream, msg, receiver)
}

// FlowStream is part of the DistSQLServer interface.
func (ds *ServerImpl) FlowStream(stream DistSQL_FlowStreamServer) error {
	ctx := ds.AnnotateCtx(stream.Context())
	err := ds.flowStreamInt(ctx, stream)
	if err != nil {
		log.Error(ctx, err)
	}
	return err
}

// TestingKnobs are the testing knobs.
type TestingKnobs struct {
	// RunBeforeBackfillChunk is called before executing each chunk of a
	// backfill during a schema change operation. It is called with the
	// current span and returns an error which eventually is returned to the
	// caller of SchemaChanger.exec(). It is called at the start of the
	// backfill function passed into the transaction executing the chunk.
	RunBeforeBackfillChunk func(sp roachpb.Span) error

	// RunAfterBackfillChunk is called after executing each chunk of a
	// backfill during a schema change operation. It is called just before
	// returning from the backfill function passed into the transaction
	// executing the chunk. It is always called even when the backfill
	// function returns an error, or if the table has already been dropped.
	RunAfterBackfillChunk func()
}

// ModuleTestingKnobs is part of the base.ModuleTestingKnobs interface.
func (*TestingKnobs) ModuleTestingKnobs() {}

// Copyright 2017 Pilosa Corp.
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

package pilosa

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pilosa/pilosa/internal"
	"github.com/pkg/errors"

	"golang.org/x/sync/errgroup"
)

// Default server settings.
const (
	DefaultDiagnosticServer = "https://diagnostics.pilosa.com/v0/diagnostics"
)

// Ensure Server implements interfaces.
var _ broadcaster = &Server{}
var _ MemberServer = &Server{}

// Server represents a holder wrapped by a running HTTP server.
type Server struct {
	// Close management.
	wg      sync.WaitGroup
	closing chan struct{}

	// Internal
	holder          *Holder
	cluster         *cluster
	translateFile   *TranslateFile
	diagnostics     *DiagnosticsCollector
	executor        *executor
	hosts           []string
	clusterDisabled bool

	// External
	systemInfo SystemInfo
	gcNotifier GCNotifier
	logger     Logger

	nodeID              string
	URI                 URI
	antiEntropyInterval time.Duration
	metricInterval      time.Duration
	diagnosticInterval  time.Duration
	maxWritesPerRequest int
	isCoordinator       bool
	syncer              holderSyncer

	primaryTranslateStore TranslateStore

	defaultClient InternalClient
	dataDir       string
}

// TODO: have this return an interface for Holder instead of concrete object?
func (s *Server) Holder() *Holder {
	return s.holder
}

// ServerOption is a functional option type for pilosa.Server
type ServerOption func(s *Server) error

func OptServerLogger(l Logger) ServerOption {
	return func(s *Server) error {
		s.logger = l
		return nil
	}
}

func OptServerReplicaN(n int) ServerOption {
	return func(s *Server) error {
		s.cluster.ReplicaN = n
		return nil
	}
}

func OptServerDataDir(dir string) ServerOption {
	return func(s *Server) error {
		s.dataDir = dir
		return nil
	}
}

func OptServerAttrStoreFunc(af func(string) AttrStore) ServerOption {
	return func(s *Server) error {
		s.holder.NewAttrStore = af
		return nil
	}
}

func OptServerAntiEntropyInterval(interval time.Duration) ServerOption {
	return func(s *Server) error {
		s.antiEntropyInterval = interval
		return nil
	}
}

func OptServerLongQueryTime(dur time.Duration) ServerOption {
	return func(s *Server) error {
		s.cluster.longQueryTime = dur
		return nil
	}
}

func OptServerMaxWritesPerRequest(n int) ServerOption {
	return func(s *Server) error {
		s.maxWritesPerRequest = n
		return nil
	}
}

func OptServerMetricInterval(dur time.Duration) ServerOption {
	return func(s *Server) error {
		s.metricInterval = dur
		return nil
	}
}

func OptServerSystemInfo(si SystemInfo) ServerOption {
	return func(s *Server) error {
		s.systemInfo = si
		return nil
	}
}

func OptServerGCNotifier(gcn GCNotifier) ServerOption {
	return func(s *Server) error {
		s.gcNotifier = gcn
		return nil
	}
}

func OptServerInternalClient(c InternalClient) ServerOption {
	return func(s *Server) error {
		s.executor = newExecutor(optExecutorInternalQueryClient(c))
		s.defaultClient = c
		s.cluster.InternalClient = c
		return nil
	}
}

func OptServerPrimaryTranslateStore(store TranslateStore) ServerOption {
	return func(s *Server) error {
		s.primaryTranslateStore = store
		return nil
	}
}

func OptServerStatsClient(sc StatsClient) ServerOption {
	return func(s *Server) error {
		s.holder.Stats = sc
		return nil
	}
}

func OptServerDiagnosticsInterval(dur time.Duration) ServerOption {
	return func(s *Server) error {
		s.diagnosticInterval = dur
		return nil
	}
}

func OptServerURI(uri *URI) ServerOption {
	return func(s *Server) error {
		s.URI = *uri
		return nil
	}
}

// OptClusterDisabled tells the server whether to use a static cluster with the
// defined hosts. Mostly used for testing.
func OptServerClusterDisabled(disabled bool, hosts []string) ServerOption {
	return func(s *Server) error {
		s.hosts = hosts
		s.clusterDisabled = disabled
		return nil
	}
}

func OptServerIsCoordinator(is bool) ServerOption {
	return func(s *Server) error {
		s.isCoordinator = is
		return nil
	}
}

func OptServerNodeID(nodeID string) ServerOption {
	return func(s *Server) error {
		s.nodeID = nodeID
		return nil
	}
}

func OptServerClusterHasher(h Hasher) ServerOption {
	return func(s *Server) error {
		s.cluster.Hasher = h
		return nil
	}
}

// NewServer returns a new instance of Server.
func NewServer(opts ...ServerOption) (*Server, error) {
	s := &Server{
		closing:     make(chan struct{}),
		cluster:     newCluster(),
		holder:      NewHolder(),
		diagnostics: NewDiagnosticsCollector(DefaultDiagnosticServer),
		systemInfo:  NewNopSystemInfo(),

		gcNotifier: NopGCNotifier,

		antiEntropyInterval: time.Minute * 10,
		metricInterval:      0,
		diagnosticInterval:  0,

		logger: NopLogger,
	}
	s.diagnostics.server = s

	for _, opt := range opts {
		err := opt(s)
		if err != nil {
			return nil, errors.Wrap(err, "applying option")
		}
	}

	path, err := expandDirName(s.dataDir)
	if err != nil {
		return nil, err
	}

	s.holder.Path = path
	s.holder.Logger = s.logger
	s.holder.Stats.SetLogger(s.logger)

	s.cluster.Path = path
	s.cluster.logger = s.logger
	s.cluster.holder = s.holder

	// Initialize translation database.
	s.translateFile = NewTranslateFile()
	s.translateFile.Path = filepath.Join(path, ".keys")
	s.translateFile.PrimaryTranslateStore = s.primaryTranslateStore

	// Get or create NodeID.
	s.nodeID = s.loadNodeID()
	if s.isCoordinator {
		s.cluster.Coordinator = s.nodeID
	}

	// Set Cluster Node.
	node := &Node{
		ID:            s.nodeID,
		URI:           s.URI,
		IsCoordinator: s.cluster.Coordinator == s.nodeID,
	}
	s.cluster.Node = node
	if s.clusterDisabled {
		err := s.cluster.setStatic(s.hosts)
		if err != nil {
			return nil, errors.Wrap(err, "setting cluster static")
		}
	}

	// Append the NodeID tag to stats.
	s.holder.Stats = s.holder.Stats.WithTags(fmt.Sprintf("NodeID:%s", s.nodeID))

	s.executor.Holder = s.holder
	s.executor.Node = node
	s.executor.Cluster = s.cluster
	s.executor.TranslateStore = s.translateFile
	s.executor.MaxWritesPerRequest = s.maxWritesPerRequest
	s.cluster.broadcaster = s
	s.cluster.maxWritesPerRequest = s.maxWritesPerRequest
	s.holder.broadcaster = s

	err = s.cluster.setup()
	if err != nil {
		return nil, errors.Wrap(err, "setting up cluster")
	}

	return s, nil
}

// Open opens and initializes the server.
func (s *Server) Open() error {
	s.logger.Printf("open server")

	// Log startup
	err := s.holder.logStartup()
	if err != nil {
		log.Println(errors.Wrap(err, "logging startup"))
	}

	// Initialize id-key storage.
	if err := s.translateFile.Open(); err != nil {
		return err
	}

	// Open Cluster management.
	if err := s.cluster.waitForStarted(); err != nil {
		return fmt.Errorf("opening Cluster: %v", err)
	}

	// Open holder.
	if err := s.holder.Open(); err != nil {
		return fmt.Errorf("opening Holder: %v", err)
	}
	if err := s.cluster.setNodeState(NodeStateReady); err != nil {
		return fmt.Errorf("setting nodeState: %v", err)
	}

	// Listen for joining nodes.
	// This needs to start after the Holder has opened so that nodes can join
	// the cluster without waiting for data to load on the coordinator. Before
	// this starts, the joins are queued up in the Cluster.joiningLeavingNodes
	// buffered channel.
	s.cluster.listenForJoins()

	s.syncer.Holder = s.holder
	s.syncer.Node = s.cluster.Node
	s.syncer.Cluster = s.cluster
	s.syncer.Closing = s.closing
	s.syncer.Stats = s.holder.Stats.WithTags("HolderSyncer")

	// Start background monitoring.
	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.monitorAntiEntropy() }()
	go func() { defer s.wg.Done(); s.monitorRuntime() }()
	go func() { defer s.wg.Done(); s.monitorDiagnostics() }()

	return nil
}

// Close closes the server and waits for it to shutdown.
func (s *Server) Close() error {
	// Notify goroutines to stop.
	close(s.closing)
	s.wg.Wait()

	if s.cluster != nil {
		s.cluster.close()
	}
	if s.holder != nil {
		s.holder.Close()
	}
	if s.translateFile != nil {
		s.translateFile.Close()
	}

	return nil
}

// loadNodeID gets NodeID from disk, or creates a new value.
// If server.NodeID is already set, a new ID is not created.
func (s *Server) loadNodeID() string {
	if s.nodeID != "" {
		return s.nodeID
	}
	nodeID, err := s.holder.loadNodeID()
	if err != nil {
		s.logger.Printf("loading NodeID: %v", err)
		return s.nodeID
	}
	return nodeID
}

// SyncData manually invokes the anti entropy process which makes sure that this
// node has the data from all replicas across the cluster.
func (s *Server) SyncData() error {
	return errors.Wrap(s.syncer.SyncHolder(), "syncing holder")
}

func (s *Server) monitorAntiEntropy() {
	if s.antiEntropyInterval == 0 {
		return // anti entropy disabled
	}
	ticker := time.NewTicker(s.antiEntropyInterval)
	defer ticker.Stop()

	s.logger.Printf("holder sync monitor initializing (%s interval)", s.antiEntropyInterval)

	// Initialize syncer with local holder and remote client.
	for {
		// Wait for tick or a close.
		select {
		case <-s.closing:
			return
		case <-ticker.C:
			s.holder.Stats.Count("AntiEntropy", 1, 1.0)
		}
		t := time.Now()

		// Sync holders.
		s.logger.Printf("holder sync beginning")
		if err := s.syncer.SyncHolder(); err != nil {
			s.logger.Printf("holder sync error: err=%s", err)
			continue
		}

		// Record successful sync in log.
		s.logger.Printf("holder sync complete")
		dif := time.Since(t)
		s.holder.Stats.Histogram("AntiEntropyDuration", float64(dif), 1.0)
	}
}

// ReceiveMessage represents an implementation of BroadcastHandler.
func (s *Server) ReceiveMessage(pb proto.Message) error {
	switch obj := pb.(type) {
	case *internal.CreateShardMessage:
		idx := s.holder.Index(obj.Index)
		if idx == nil {
			return fmt.Errorf("Local Index not found: %s", obj.Index)
		}
		idx.SetRemoteMaxShard(obj.Shard)
	case *internal.CreateIndexMessage:
		opt := IndexOptions{}
		_, err := s.holder.CreateIndex(obj.Index, opt)
		if err != nil {
			return err
		}
	case *internal.DeleteIndexMessage:
		if err := s.holder.DeleteIndex(obj.Index); err != nil {
			return err
		}
	case *internal.CreateFieldMessage:
		idx := s.holder.Index(obj.Index)
		if idx == nil {
			return fmt.Errorf("Local Index not found: %s", obj.Index)
		}
		opt := decodeFieldOptions(obj.Meta)
		_, err := idx.CreateField(obj.Field, *opt)
		if err != nil {
			return err
		}
	case *internal.DeleteFieldMessage:
		idx := s.holder.Index(obj.Index)
		if err := idx.DeleteField(obj.Field); err != nil {
			return err
		}
	case *internal.CreateViewMessage:
		f := s.holder.Field(obj.Index, obj.Field)
		if f == nil {
			return fmt.Errorf("Local Field not found: %s", obj.Field)
		}
		_, _, err := f.createViewIfNotExistsBase(obj.View)
		if err != nil {
			return err
		}
	case *internal.DeleteViewMessage:
		f := s.holder.Field(obj.Index, obj.Field)
		if f == nil {
			return fmt.Errorf("Local Field not found: %s", obj.Field)
		}
		err := f.deleteView(obj.View)
		if err != nil {
			return err
		}
	case *internal.ClusterStatus:
		err := s.cluster.mergeClusterStatus(obj)
		if err != nil {
			return err
		}
	case *internal.ResizeInstruction:
		err := s.cluster.followResizeInstruction(obj)
		if err != nil {
			return err
		}
	case *internal.ResizeInstructionComplete:
		err := s.cluster.markResizeInstructionComplete(obj)
		if err != nil {
			return err
		}
	case *internal.SetCoordinatorMessage:
		s.cluster.setCoordinator(DecodeNode(obj.New))
	case *internal.UpdateCoordinatorMessage:
		s.cluster.updateCoordinator(DecodeNode(obj.New))
	case *internal.NodeStateMessage:
		err := s.cluster.receiveNodeState(obj.NodeID, obj.State)
		if err != nil {
			return err
		}
	case *internal.RecalculateCaches:
		s.holder.RecalculateCaches()
	case *internal.NodeEventMessage:
		s.cluster.ReceiveEvent(DecodeNodeEvent(obj))
	}

	return nil
}

// SendSync represents an implementation of Broadcaster.
func (s *Server) SendSync(pb proto.Message) error {
	var eg errgroup.Group
	for _, node := range s.cluster.Nodes {
		node := node
		s.logger.Printf("SendSync to: %s", node.URI)
		// Don't forward the message to ourselves.
		if s.URI == node.URI {
			continue
		}

		eg.Go(func() error {
			return s.defaultClient.SendMessage(context.Background(), &node.URI, pb)
		})
	}

	return eg.Wait()
}

// SendAsync represents an implementation of Broadcaster.
func (s *Server) SendAsync(pb proto.Message) error {
	return ErrNotImplemented
}

// SendTo represents an implementation of Broadcaster.
func (s *Server) SendTo(to *Node, pb proto.Message) error {
	s.logger.Printf("SendTo: %s", to.URI)
	return s.defaultClient.SendMessage(context.Background(), &to.URI, pb)
}

// Node returns the pilosa.Node object. It is used by membership protocols to
// get this node's name(ID), location(URI), and coordinator status.
func (s *Server) Node() *Node {
	return s.cluster.Node
}

// Server implements StatusHandler.
// LocalStatus is used to periodically sync information
// between nodes. Under normal conditions, nodes should
// remain in sync through Broadcast messages. For cases
// where a node fails to receive a Broadcast message, or
// when a new (empty) node needs to get in sync with the
// rest of the cluster, two things are shared via gossip:
// - MaxShard by Index
// - Schema
// In a gossip implementation, memberlist.Delegate.LocalState() uses this.
func (s *Server) LocalStatus() (proto.Message, error) {
	if s.cluster == nil {
		return nil, errors.New("Server.Cluster is nil")
	}
	if s.holder == nil {
		return nil, errors.New("Server.Holder is nil")
	}

	ns := internal.NodeStatus{
		Node:      EncodeNode(s.cluster.Node),
		MaxShards: s.holder.encodeMaxShards(),
		Schema:    s.holder.encodeSchema(),
	}

	return &ns, nil
}

// HandleRemoteStatus receives incoming NodeStatus from remote nodes.
func (s *Server) HandleRemoteStatus(pb proto.Message) error {
	// Ignore NodeStatus messages until the cluster is in a Normal state.
	if s.cluster.State() != ClusterStateNormal {
		return nil
	}

	go func() {
		// Make sure the holder has opened.
		<-s.holder.opened

		err := s.mergeRemoteStatus(pb.(*internal.NodeStatus))
		if err != nil {
			s.logger.Printf("merge remote status: %s", err)
		}
	}()

	return nil
}

func (s *Server) mergeRemoteStatus(ns *internal.NodeStatus) error {
	// Ignore status updates from self.
	if s.nodeID == DecodeNode(ns.Node).ID {
		return nil
	}

	// Sync schema.
	if err := s.holder.applySchema(ns.Schema); err != nil {
		return errors.Wrap(err, "applying schema")
	}

	// Sync maxShards.
	oldmaxshards := s.holder.maxShards()
	for index, newMax := range ns.MaxShards.Standard {
		localIndex := s.holder.Index(index)
		// if we don't know about an index locally, log an error because
		// indexes should be created and synced prior to shard creation
		if localIndex == nil {
			s.logger.Printf("Local Index not found: %s", index)
			continue
		}
		if newMax > oldmaxshards[index] {
			oldmaxshards[index] = newMax
			localIndex.SetRemoteMaxShard(newMax)
		}
	}

	return nil
}

// monitorDiagnostics periodically polls the Pilosa Indexes for cluster info.
func (s *Server) monitorDiagnostics() {
	// Do not send more than once a minute
	if s.diagnosticInterval < time.Minute {
		s.logger.Printf("diagnostics disabled")
		return
	} else {
		s.logger.Printf("Pilosa is currently configured to send small diagnostics reports to our team every %v. More information here: https://www.pilosa.com/docs/latest/administration/#diagnostics", s.diagnosticInterval)
	}

	s.diagnostics.Logger = s.logger
	s.diagnostics.SetVersion(Version)
	s.diagnostics.Set("Host", s.URI.host)
	s.diagnostics.Set("Cluster", strings.Join(s.cluster.nodeIDs(), ","))
	s.diagnostics.Set("NumNodes", len(s.cluster.Nodes))
	s.diagnostics.Set("NumCPU", runtime.NumCPU())
	s.diagnostics.Set("NodeID", s.nodeID)
	s.diagnostics.Set("ClusterID", s.cluster.id)
	s.diagnostics.EnrichWithOSInfo()

	// Flush the diagnostics metrics at startup, then on each tick interval
	flush := func() {
		openFiles, err := countOpenFiles()
		if err == nil {
			s.diagnostics.Set("OpenFiles", openFiles)
		}
		s.diagnostics.Set("GoRoutines", runtime.NumGoroutine())
		s.diagnostics.EnrichWithMemoryInfo()
		s.diagnostics.EnrichWithSchemaProperties()
		s.diagnostics.CheckVersion()
		err = s.diagnostics.Flush()
		if err != nil {
			s.logger.Printf("Diagnostics error: %s", err)
		}
	}

	ticker := time.NewTicker(s.diagnosticInterval)
	defer ticker.Stop()
	flush()
	for {
		// Wait for tick or a close.
		select {
		case <-s.closing:
			return
		case <-ticker.C:
			flush()
		}
	}
}

// monitorRuntime periodically polls the Go runtime metrics.
func (s *Server) monitorRuntime() {
	// Disable metrics when poll interval is zero.
	if s.metricInterval <= 0 {
		return
	}

	var m runtime.MemStats
	ticker := time.NewTicker(s.metricInterval)
	defer ticker.Stop()

	defer s.gcNotifier.Close()

	s.logger.Printf("runtime stats initializing (%s interval)", s.metricInterval)

	for {
		// Wait for tick or a close.
		select {
		case <-s.closing:
			return
		case <-s.gcNotifier.AfterGC():
			// GC just ran.
			s.holder.Stats.Count("garbage_collection", 1, 1.0)
		case <-ticker.C:
		}

		// Record the number of go routines.
		s.holder.Stats.Gauge("goroutines", float64(runtime.NumGoroutine()), 1.0)

		openFiles, err := countOpenFiles()
		// Open File handles.
		if err == nil {
			s.holder.Stats.Gauge("OpenFiles", float64(openFiles), 1.0)
		}

		// Runtime memory metrics.
		runtime.ReadMemStats(&m)
		s.holder.Stats.Gauge("HeapAlloc", float64(m.HeapAlloc), 1.0)
		s.holder.Stats.Gauge("HeapInuse", float64(m.HeapInuse), 1.0)
		s.holder.Stats.Gauge("StackInuse", float64(m.StackInuse), 1.0)
		s.holder.Stats.Gauge("Mallocs", float64(m.Mallocs), 1.0)
		s.holder.Stats.Gauge("Frees", float64(m.Frees), 1.0)
	}
}

// countOpenFiles on operating systems that support lsof.
func countOpenFiles() (int, error) {
	switch runtime.GOOS {
	case "darwin", "linux", "unix", "freebsd":
		// -b option avoid kernel blocks
		pid := os.Getpid()
		out, err := exec.Command("/bin/sh", "-c", fmt.Sprintf("lsof -b -p %v", pid)).Output()
		if err != nil {
			return 0, fmt.Errorf("calling lsof: %s", err)
		}
		// only count lines with our pid, avoiding warning messages from -b
		lines := strings.Split(string(out), strconv.Itoa(pid))
		return len(lines), nil
	case "windows":
		// TODO: count open file handles on windows
		return 0, errors.New("countOpenFiles() on Windows is not supported")
	default:
		return 0, errors.New("countOpenFiles() on this OS is not supported")
	}
}

func expandDirName(path string) (string, error) {
	prefix := "~" + string(filepath.Separator)
	if strings.HasPrefix(path, prefix) {
		HomeDir := os.Getenv("HOME")
		if HomeDir == "" {
			return "", errors.New("data directory not specified and no home dir available")
		}
		return filepath.Join(HomeDir, strings.TrimPrefix(path, prefix)), nil
	}
	return path, nil
}

type MemberServer interface {
	ReceiveMessage(proto.Message) error
	LocalStatus() (proto.Message, error)
	HandleRemoteStatus(proto.Message) error
	Node() *Node
}
